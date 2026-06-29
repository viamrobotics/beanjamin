package beanjamin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	viz "github.com/viam-labs/motion-tools/client/client"
	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/components/gripper"
	"go.viam.com/rdk/components/sensor"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/robot/framesystem"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/services/vision"
	"go.viam.com/rdk/spatialmath"

	// Register the multi-poses-execution-switch model.
	_ "beanjamin/multiposesexecutionswitch"
)

var Coffee = resource.NewModel("viam", "beanjamin", "coffee")

func init() {
	resource.RegisterService(generic.API, Coffee,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newBeanjaminCoffee,
		},
	)
}

type beanjaminCoffee struct {
	resource.AlwaysRebuild

	name                   resource.Name
	logger                 logging.Logger
	cfg                    *Config
	filterSw               toggleswitch.Switch
	clawsSw                toggleswitch.Switch
	cameraObserveSw        toggleswitch.Switch // holds the camera observation vantages for cup pickup.
	arm                    arm.Arm
	fsSvc                  framesystem.Service
	cachedFS               *referenceframe.FrameSystem // cached frame system, mutated at lock/unlock
	speech                 resource.Resource           // nil when speech_service_name is not configured
	vizEnabled             bool                        // true when viz_url is configured
	vizConsecutiveFailures int                         // auto-disables viz after repeated failures
	gripper                gripper.Gripper
	camStorage             generic.Service // optional; mux over video stores; nil if cam_storage_mux_name unset
	iceBoard               board.Board     // optional; drives the ice-machine GPIO pin; nil if ice_board_name unset
	slackNotifier          generic.Service // optional; viam:notifications:slack; nil if slack_notifier_name unset
	customerDetector       generic.Service // optional; viam:beanjamin:customer-detector; nil if customer_detector_name unset
	machineLogsURL         string          // app.viam.com logs deep-link from VIAM_MACHINE_ID/VIAM_PRIMARY_ORG_ID env; "" when unavailable (e.g. local/test machine)
	dataLocationID         string          // VIAM_LOCATION_ID env; used to build per-order clip data-page links; "" when unavailable
	pendingOrderClipsDir   string          // optional; directory for pending-clip records to survive restarts
	mu                     sync.Mutex
	cancelCtx              context.Context
	cancelFunc             func()
	running                atomic.Bool
	currentStep            atomic.Value // string: current step label for the active order (debug)
	// failedStep holds the step label the most recent order errored at,
	// captured inside prepareDrink before `running` flips false so cancel
	// recovery can't overwrite it. "" when the order succeeded. Reported on
	// the order sensor; reset at the start of each order.
	failedStep     atomic.Value
	currentOrderID atomic.Value // string: ID of the order currently being processed; "" when idle
	// activeLogger holds the order-scoped logger (tagged with order_id) for the
	// order currently being processed; set by processQueue and cleared when it
	// finishes. Entry points that run outside the queue goroutine — notably
	// cancel — read it via activeOrderLogger() so their logs carry the in-flight
	// order's order_id. nil when idle.
	activeLogger atomic.Pointer[logging.Logger]
	queue        *OrderQueue
	queueStop    chan struct{}
	paused       atomic.Bool
	// portafilterInMachine is true between releaseFilter and grabFilter:
	// the bayonet holds the filter and the arm is free. Cancel uses this
	// to decide whether recovery (re-grip + clean + home) is required.
	portafilterInMachine atomic.Bool
	// portafilterHasGrounds is true once grinding has put grounds in the
	// filter, until cleanPortafilter clears them. Cancel uses this (when
	// portafilterInMachine is false) to drive a clean + home recovery so
	// the filter doesn't get stranded with grounds in it.
	portafilterHasGrounds atomic.Bool
	orderSensorSink       orderSensorSink // optional; named order-sensor from deps, nil if unset
	// Optional usage sensor updated during the brew lifecycle (sensor_usage.go).
	// nil when usage_sensor_name is unset, in which case every update is a
	// no-op. Holds all counters keyed by regular_grinds, decaf_grinds, usage,
	// cleanings, and successful_consecutive_orders.
	usageSensor    sensor.Sensor
	cupVision      vision.Service // vision service for cup pickup (always configured)
	cupCameraName  string         // SrcCameraName, validated to exist in cachedFS
	glassVision    vision.Service // optional; nil unless CanServeIced
	glassObserveSw toggleswitch.Switch
	// servingAreaSlotCounter is the round-robin counter for serving-area placement.
	// It increments once per placeFullCupOnShelf and selects the shelf slot
	// modulo the number of tiles. Process-local; resets to 0 on rebuild.
	servingAreaSlotCounter atomic.Uint64

	// Held-item geometry tracking (track_held_geometry, held_geometry.go).
	// heldCupGeom / heldGlassGeom cache the gripper-local geometry of the cup /
	// glass detected at pickup so a re-grab can restore it; heldItemAttached
	// tracks whether the held-item frame is currently present in cachedFS. These
	// are mutated only on the motion sequence goroutine (like cachedFS, gated by
	// the running flag), so they need no extra locking.
	heldCupGeom      spatialmath.Geometry
	heldGlassGeom    spatialmath.Geometry
	heldItemAttached bool

	// filterFrameLocked tracks whether lockFilterFrame has re-parented the filter
	// frame to world in cachedFS (i.e. an in-flight lock that must be preserved).
	// Mutated only on the motion sequence goroutine, like cachedFS.
	filterFrameLocked bool

	// stagedGlassPlaced tracks whether stageGlassAsObstacle has added the released
	// glass geometry to world in cachedFS
	stagedGlassPlaced bool
}

func newBeanjaminCoffee(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}
	return NewCoffee(ctx, deps, rawConf.ResourceName(), conf, logger)
}

func NewCoffee(ctx context.Context, deps resource.Dependencies, name resource.Name, conf *Config, logger logging.Logger) (resource.Resource, error) {
	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	switchRes, ok := deps[toggleswitch.Named(conf.PoseSwitcherName)]
	if !ok {
		cancelFunc()
		return nil, fmt.Errorf("switch %q not found in dependencies", conf.PoseSwitcherName)
	}
	filterSw, ok := switchRes.(toggleswitch.Switch)
	if !ok {
		cancelFunc()
		return nil, fmt.Errorf("resource %q is not a switch", conf.PoseSwitcherName)
	}

	clawSwRes, ok := deps[toggleswitch.Named(conf.ClawsPoseSwitcherName)]
	if !ok {
		cancelFunc()
		return nil, fmt.Errorf("claws switch %q not found in dependencies", conf.ClawsPoseSwitcherName)
	}
	clawSw, ok := clawSwRes.(toggleswitch.Switch)
	if !ok {
		cancelFunc()
		return nil, fmt.Errorf("resource %q is not a switch", conf.ClawsPoseSwitcherName)
	}

	armComp, err := arm.FromProvider(deps, conf.ArmName)
	if err != nil {
		cancelFunc()
		return nil, fmt.Errorf("arm %q not found in dependencies: %w", conf.ArmName, err)
	}

	gripperComp, err := gripper.FromProvider(deps, conf.GripperName)
	if err != nil {
		cancelFunc()
		return nil, fmt.Errorf("gripper %q not found in dependencies: %w", conf.GripperName, err)
	}

	fsSvc, err := framesystem.FromDependencies(deps)
	if err != nil {
		cancelFunc()
		return nil, fmt.Errorf("frame system service not found in dependencies: %w", err)
	}

	cachedFS, err := framesystem.NewFromService(ctx, fsSvc, nil)
	if err != nil {
		cancelFunc()
		return nil, fmt.Errorf("build initial frame system: %w", err)
	}

	if err := applyJointLimits(logger, cachedFS, conf.InputRangeOverride); err != nil {
		cancelFunc()
		return nil, fmt.Errorf("apply joint limits: %w", err)
	}

	// Cup pickup is always vision-driven.
	cupVision, err := vision.FromProvider(deps, conf.CupVisionServiceName)
	if err != nil {
		cancelFunc()
		return nil, fmt.Errorf("cup vision service %q: %w", conf.CupVisionServiceName, err)
	}
	if cachedFS.Frame(conf.SrcCameraName) == nil {
		cancelFunc()
		return nil, fmt.Errorf("src_camera_name %q not found in frame system — add the camera to the frame system fragment", conf.SrcCameraName)
	}
	cupCameraName := conf.SrcCameraName

	observeSwRes, ok := deps[toggleswitch.Named(conf.CameraObservePoseSwitcherName)]
	if !ok {
		cancelFunc()
		return nil, fmt.Errorf("camera observe switch %q not found in dependencies", conf.CameraObservePoseSwitcherName)
	}
	cameraObserveSw, ok := observeSwRes.(toggleswitch.Switch)
	if !ok {
		cancelFunc()
		return nil, fmt.Errorf("resource %q is not a switch", conf.CameraObservePoseSwitcherName)
	}
	logger.Infof("cup vision pickup (vision=%q, camera=%q, observe_switch=%q)",
		conf.CupVisionServiceName, conf.SrcCameraName, conf.CameraObservePoseSwitcherName)

	// Iced coffee fetches a glass via its own vision pipeline (shares the cup
	// camera resolved above).
	var glassVision vision.Service
	var glassObserveSw toggleswitch.Switch
	if conf.CanServeIced {
		glassVision, err = vision.FromProvider(deps, conf.GlassVisionServiceName)
		if err != nil {
			cancelFunc()
			return nil, fmt.Errorf("glass vision service %q: %w", conf.GlassVisionServiceName, err)
		}

		obsSwRes, ok := deps[toggleswitch.Named(conf.GlassObservePoseSwitcherName)]
		if !ok {
			cancelFunc()
			return nil, fmt.Errorf("glass observe switch %q not found in dependencies", conf.GlassObservePoseSwitcherName)
		}
		glassObserveSw, ok = obsSwRes.(toggleswitch.Switch)
		if !ok {
			cancelFunc()
			return nil, fmt.Errorf("resource %q is not a switch", conf.GlassObservePoseSwitcherName)
		}
		logger.Infof("iced coffee glass vision pickup (vision=%q, camera=%q, observe_switch=%q)",
			conf.GlassVisionServiceName, conf.SrcCameraName, conf.GlassObservePoseSwitcherName)
	}

	var speech resource.Resource
	if conf.SpeechServiceName != "" {
		speechRes, ok := deps[generic.Named(conf.SpeechServiceName)]
		if ok {
			speech = speechRes
		}
		if speech != nil {
			logger.Infof("speech service %q connected", conf.SpeechServiceName)
		} else {
			logger.Warnf("speech service %q configured but not available", conf.SpeechServiceName)
		}
	}

	var camStorage generic.Service
	if conf.CamStorageMuxName != "" {
		mux, err := generic.FromProvider(deps, conf.CamStorageMuxName)
		if err != nil {
			cancelFunc()
			return nil, fmt.Errorf("cam_storage_mux_name %q: %w", conf.CamStorageMuxName, err)
		}
		camStorage = mux
		logger.Infof("cam storage mux %q connected", conf.CamStorageMuxName)
	}

	var iceBoard board.Board
	if conf.IceDispenseBoardName != "" {
		b, err := board.FromProvider(deps, conf.IceDispenseBoardName)
		if err != nil {
			cancelFunc()
			return nil, fmt.Errorf("ice_board_name %q: %w", conf.IceDispenseBoardName, err)
		}
		iceBoard = b
		logger.Infof("ice board %q connected (pin %q)", conf.IceDispenseBoardName, conf.IceDispensePinName)
	}

	var slackNotifier generic.Service
	if conf.SlackNotifierName != "" {
		notifier, err := generic.FromProvider(deps, conf.SlackNotifierName)
		if err != nil {
			cancelFunc()
			return nil, fmt.Errorf("slack_notifier_name %q: %w", conf.SlackNotifierName, err)
		}
		slackNotifier = notifier
		logger.Infof("slack notifier %q connected", conf.SlackNotifierName)
	}

	var customerDetector generic.Service
	if conf.CustomerDetectorName != "" {
		detector, err := generic.FromProvider(deps, conf.CustomerDetectorName)
		if err != nil {
			cancelFunc()
			return nil, fmt.Errorf("customer_detector_name %q: %w", conf.CustomerDetectorName, err)
		}
		customerDetector = detector
		logger.Infof("customer detector %q connected — order history recording enabled", conf.CustomerDetectorName)
	}

	var pendingOrderClipsDir string
	if conf.DataDir != "" {
		pendingOrderClipsDir = filepath.Join(conf.DataDir, "pending-clips")
		if err := os.MkdirAll(pendingOrderClipsDir, 0o755); err != nil {
			cancelFunc()
			return nil, fmt.Errorf("data_dir %q: %w", conf.DataDir, err)
		}
		logger.Infof("cam storage: pending-clip records will be written to %s", pendingOrderClipsDir)
	} else {
		logger.Infof("cam storage: no data_dir configured — pending-clip records disabled (interrupted orders will not be recoverable)")
	}

	vizEnabled := false
	if conf.VizURL != "" {
		viz.SetURL(conf.VizURL)
		vizEnabled = true
		logger.Infof("viz client configured at %s", conf.VizURL)
	}

	var sink orderSensorSink
	if conf.OrderSensorName != "" {
		// Same component instance as elsewhere on the robot (not a copy).
		sen, err := sensor.FromProvider(deps, conf.OrderSensorName)
		if err != nil {
			cancelFunc()
			return nil, fmt.Errorf("order sensor %q: %w", conf.OrderSensorName, err)
		}
		s, ok := sen.(orderSensorSink)
		if !ok {
			cancelFunc()
			return nil, fmt.Errorf("resource %q must be model viam:beanjamin:order-sensor", conf.OrderSensorName)
		}
		sink = s
		logger.Infof("order sensor %q connected", conf.OrderSensorName)
	}

	// Optional usage sensor. Resolve to the same component instance on the
	// robot; a configured-but-unresolvable name fails construction to surface
	// misconfiguration early (an unset name simply stays nil).
	var usageSensor sensor.Sensor
	if conf.UsageSensorName != "" {
		usageSensor, err = sensor.FromProvider(deps, conf.UsageSensorName)
		if err != nil {
			cancelFunc()
			return nil, fmt.Errorf("usage_sensor_name %q: %w", conf.UsageSensorName, err)
		}
		logger.Infof("usage sensor %q connected", conf.UsageSensorName)
	}

	s := &beanjaminCoffee{
		name:                 name,
		logger:               logger,
		cfg:                  conf,
		filterSw:             filterSw,
		clawsSw:              clawSw,
		cameraObserveSw:      cameraObserveSw,
		arm:                  armComp,
		fsSvc:                fsSvc,
		cachedFS:             cachedFS,
		speech:               speech,
		camStorage:           camStorage,
		iceBoard:             iceBoard,
		slackNotifier:        slackNotifier,
		customerDetector:     customerDetector,
		machineLogsURL:       buildMachineLogsURL(os.Getenv("VIAM_MACHINE_ID"), os.Getenv("VIAM_PRIMARY_ORG_ID")),
		dataLocationID:       os.Getenv("VIAM_LOCATION_ID"),
		pendingOrderClipsDir: pendingOrderClipsDir,
		gripper:              gripperComp,
		vizEnabled:           vizEnabled,
		cancelCtx:            cancelCtx,
		cancelFunc:           cancelFunc,
		queue:                NewOrderQueue(),
		queueStop:            make(chan struct{}),
		orderSensorSink:      sink,
		usageSensor:          usageSensor,
		cupVision:            cupVision,
		cupCameraName:        cupCameraName,
		glassVision:          glassVision,
		glassObserveSw:       glassObserveSw,
	}

	// Fail fast if the enabled configuration references poses that are missing
	// from (or unset on) the switches, rather than discovering it mid-order.
	if err := s.validateConfiguredPoses(ctx); err != nil {
		cancelFunc()
		return nil, err
	}

	go s.processQueue()
	return s, nil
}

func (s *beanjaminCoffee) Name() resource.Name {
	return s.name
}

// resetCancelWaitTimeout caps how long resetWorld waits for a running sequence
// to observe its cancelled context and return. Generous enough to cover any
// motion-plan cleanup; if exceeded, something is wedged and the operator
// should look at logs rather than have reset_world appear to "succeed".
const resetCancelWaitTimeout = 30 * time.Second

const cancelAnnouncement = "Cancelling the current order. I'll clean up if needed and return to home. Click proceed when you're ready for the next order."

func (s *beanjaminCoffee) Close(context.Context) error {
	close(s.queueStop)
	s.cancelFunc()
	return nil
}
