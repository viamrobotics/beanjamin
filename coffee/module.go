// Package coffee registers the viam:beanjamin:coffee generic service, which
// orchestrates a Viam arm through a full espresso brew cycle, together with the
// viam:beanjamin:order-sensor component that records one observability reading
// per completed order attempt.
package coffee

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/components/gripper"
	"go.viam.com/rdk/components/sensor"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/robot/framesystem"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/services/vision"
	"go.viam.com/rdk/spatialmath"

	// Register the multi-poses-execution-switch model.
	_ "beanjamin/multiposesexecutionswitch"
)

var Model = resource.NewModel("viam", "beanjamin", "coffee")

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newBeanjaminCoffee,
		},
	)
}

type beanjaminCoffee struct {
	resource.AlwaysRebuild
	// Supplies QueryTabularDataForResource, which reads this machine's own
	// synced tabular data back out of the cloud (daily_summary.go). It
	// authenticates from the VIAM_API_KEY/VIAM_API_KEY_ID env vars, which
	// viam-server only injects when the machine config carries an api-key auth
	// handler — without one the query fails at call time rather than here.
	module.ResourceDataConsumer

	name                 resource.Name
	logger               logging.Logger
	cfg                  *Config
	filterSw             toggleswitch.Switch
	clawsSw              toggleswitch.Switch
	cameraObserveSw      toggleswitch.Switch // holds the camera observation vantages for cup pickup.
	arm                  arm.Arm
	fsSvc                framesystem.Service
	cachedFS             *referenceframe.FrameSystem // cached frame system, mutated at lock/unlock
	speech               resource.Resource           // nil when speech_service_name is not configured
	gripper              gripper.Gripper
	camStorage           generic.Service // optional; mux over video stores; nil if cam_storage_mux_name unset
	iceBoard             board.Board     // optional; drives the ice-machine GPIO pin; nil if ice_board_name unset
	slackNotifier        generic.Service // optional; viam:notifications:slack; nil if slack_notifier_name unset
	customerDetector     generic.Service // optional; viam:beanjamin:customer-detector; nil if customer_detector_name unset
	deliveryHandler      generic.Service // optional; peer-machine service reached via a remote; nil if delivery_handler_name unset
	machineLogsURL       string          // app.viam.com logs deep-link from VIAM_MACHINE_ID/VIAM_PRIMARY_ORG_ID env; "" when unavailable (e.g. local/test machine)
	dataLocationID       string          // VIAM_LOCATION_ID env; used to build per-order clip data-page links; "" when unavailable
	primaryOrgID         string          // VIAM_PRIMARY_ORG_ID env; scopes app.viam.com deep-links to the owning org; "" when unavailable
	pendingOrderClipsDir string          // optional; directory for pending-clip records to survive restarts
	// lease owns the arm: who holds it, how to cancel them, and the queue
	// pause (arm_lease.go).
	lease       armLease
	currentStep atomic.Value // string: current step label for the active order (debug)
	// failedStep holds the step label the most recent order errored at,
	// captured inside prepareDrink while the queue still holds the arm lease so
	// cancel recovery can't overwrite it. "" when the order succeeded. Reported on
	// the order sensor; reset at the start of each order.
	failedStep atomic.Value
	// faultActive is raised for faultWindow after a genuine fault and surfaced
	// in Status() as fault_active (fault_alert.go).
	faultActive atomic.Bool
	// activeLogger holds the order-scoped logger (tagged with order_id) for the
	// order currently being processed; set by processQueue and cleared when it
	// finishes. Entry points that run outside the queue goroutine — cancel and
	// rewind — read it via activeOrderLogger() so their logs carry the in-flight
	// order's order_id. nil when idle.
	activeLogger atomic.Pointer[logging.Logger]
	queue        *OrderQueue
	queueStop    chan struct{}
	// portafilterInMachine is true between releaseFilter and grabFilter:
	// the bayonet holds the filter and the arm is free. Rewind uses this
	// to decide whether recovery (re-grip + clean + home) is required.
	portafilterInMachine atomic.Bool
	// portafilterHasGrounds is true once grinding has put grounds in the
	// filter, until cleanPortafilter clears them. Rewind uses this (when
	// portafilterInMachine is false) to drive a clean + home recovery so
	// the filter doesn't get stranded with grounds in it.
	portafilterHasGrounds atomic.Bool
	orderSensorSink       orderSensorSink // optional; named order-sensor from deps, nil if unset
	// Optional usage sensor updated during the brew lifecycle (sensor_usage.go).
	// nil when usage_sensor_name is unset, in which case every update is a
	// no-op. Holds all counters keyed by regular_grinds, decaf_grinds, usage,
	// cleanings, espresso_cups_used, latte_glasses_used, and
	// successful_consecutive_orders.
	usageSensor sensor.Sensor
	// machineActivity is when water last ran through the espresso machine, driving
	// the keep-alive loop (keepalive.go). nil when keepalive is unconfigured.
	machineActivity *machineActivityStore
	cupVision       vision.Service // vision service for cup pickup (always configured)
	cupCameraName   string         // SrcCameraName, validated to exist in cachedFS
	// srcCamera is the same camera as cupCameraName, held as a resource so the
	// ice-level measurement (ice_level.go) can read frames directly. The vision
	// pipelines reach it by name through their own services instead.
	srcCamera      camera.Camera
	glassVision    vision.Service // optional; nil unless CanServeIced
	glassObserveSw toggleswitch.Switch
	milkVision     vision.Service // optional; nil unless CanServeIcedLatte
	milkObserveSw  toggleswitch.Switch
	// servingAreaSlotCounter is the round-robin counter for serving-area placement.
	// It increments once per placeFullCupOnShelf and selects the shelf slot
	// modulo the number of tiles. Process-local; resets to 0 on rebuild.
	servingAreaSlotCounter atomic.Uint64

	// Held-item geometry tracking (held_geometry.go).
	// heldCupGeom / heldGlassGeom / heldMilkGeom cache the gripper-local geometry
	// of the cup / glass / milk bottle detected at pickup so a re-grab can restore
	// it; heldItemAttached tracks whether the held-item frame is currently present
	// in cachedFS. These are mutated only on the motion sequence goroutine (like
	// cachedFS, gated by the arm lease), so they need no extra locking.
	heldCupGeom      spatialmath.Geometry
	heldGlassGeom    spatialmath.Geometry
	heldMilkGeom     spatialmath.Geometry
	heldItemAttached bool

	// milkGraspCentroid is the world-frame centroid the milk bottle was grasped
	// at, recorded by fetchMilkBottle and replayed by returnMilkBottle to set the
	// bottle back down where it came from (milk.go). nil when no bottle is held.
	// Mutated only on the motion sequence goroutine, like cachedFS.
	milkGraspCentroid *r3.Vector

	// filterFrameLocked tracks whether lockFilterFrame has re-parented the filter
	// frame to world in cachedFS (i.e. an in-flight lock that must be preserved).
	// Mutated only on the motion sequence goroutine, like cachedFS.
	filterFrameLocked bool

	// stagedGlassPlaced tracks whether stageGlassAsObstacle has added the released
	// glass geometry to world in cachedFS
	stagedGlassPlaced bool

	// doorOpenDegs is how far the fridge door physically stands open, in degrees
	// about its hinge (0 = shut). The frame system always rebuilds with the door
	// at its authored shut transform, so this is the only record that survives a
	// rebuild — resetFrameSystem re-applies it, keeping the modeled panel where
	// the real one actually is instead of snapping it closed behind the arm's
	// back. Cleared only by the two commands in which an operator asserts the
	// world is as configured — reset_world and proceed — and never by a rebuild
	// on its own. Mutated only on the motion sequence goroutine, like cachedFS.
	doorOpenDegs float64
}

func newBeanjaminCoffee(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}
	return NewCoffee(ctx, deps, rawConf.ResourceName(), conf, logger)
}

// switchDep resolves a pose switch by its configured name. configKey names the
// config field in errors so a misconfiguration points at what to edit.
func switchDep(deps resource.Dependencies, configKey, name string) (toggleswitch.Switch, error) {
	res, ok := deps[toggleswitch.Named(name)]
	if !ok {
		return nil, fmt.Errorf("%s: switch %q not found in dependencies", configKey, name)
	}
	sw, ok := res.(toggleswitch.Switch)
	if !ok {
		return nil, fmt.Errorf("%s: resource %q is not a switch", configKey, name)
	}
	return sw, nil
}

// optionalGenericDep resolves an optional generic service. An unset name yields
// (nil, nil) — the feature is simply off — but a name that is set and does not
// resolve fails construction, so a typo surfaces at config time rather than as a
// silently disabled feature.
func optionalGenericDep(deps resource.Dependencies, logger logging.Logger, configKey, name, enables string) (generic.Service, error) {
	if name == "" {
		return nil, nil
	}
	svc, err := generic.FromProvider(deps, name)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", configKey, name, err)
	}
	logger.Infof("%s %q connected%s", configKey, name, enables)
	return svc, nil
}

// visionPickup resolves the vision service and observe-pose switch backing one
// vision-driven pickup (cup, glass, or milk bottle). All three share the cup
// camera, so only the per-target pair is resolved here.
func visionPickup(deps resource.Dependencies, logger logging.Logger, label, visionName, switchName, cameraName string) (vision.Service, toggleswitch.Switch, error) {
	vis, err := vision.FromProvider(deps, visionName)
	if err != nil {
		return nil, nil, fmt.Errorf("%s vision service %q: %w", label, visionName, err)
	}
	sw, err := switchDep(deps, label+" observe switch", switchName)
	if err != nil {
		return nil, nil, err
	}
	logger.Infof("%s vision pickup (vision=%q, camera=%q, observe_switch=%q)", label, visionName, cameraName, switchName)
	return vis, sw, nil
}

func NewCoffee(ctx context.Context, deps resource.Dependencies, name resource.Name, conf *Config, logger logging.Logger) (resource.Resource, error) {
	filterSw, err := switchDep(deps, "pose_switcher_name", conf.PoseSwitcherName)
	if err != nil {
		return nil, err
	}
	clawSw, err := switchDep(deps, "claws_pose_switcher_name", conf.ClawsPoseSwitcherName)
	if err != nil {
		return nil, err
	}

	armComp, err := arm.FromProvider(deps, conf.ArmName)
	if err != nil {
		return nil, fmt.Errorf("arm %q not found in dependencies: %w", conf.ArmName, err)
	}

	gripperComp, err := gripper.FromProvider(deps, conf.GripperName)
	if err != nil {
		return nil, fmt.Errorf("gripper %q not found in dependencies: %w", conf.GripperName, err)
	}

	fsSvc, err := framesystem.FromDependencies(deps)
	if err != nil {
		return nil, fmt.Errorf("frame system service not found in dependencies: %w", err)
	}

	cachedFS, err := framesystem.NewFromService(ctx, fsSvc, nil)
	if err != nil {
		return nil, fmt.Errorf("build initial frame system: %w", err)
	}

	if err := applyJointLimits(logger, cachedFS, conf.InputRangeOverride); err != nil {
		return nil, fmt.Errorf("apply joint limits: %w", err)
	}

	// The camera backs every vision pickup, so it is checked once here.
	if cachedFS.Frame(conf.SrcCameraName) == nil {
		return nil, fmt.Errorf("src_camera_name %q not found in frame system — add the camera to the frame system fragment", conf.SrcCameraName)
	}

	// Cup pickup is always vision-driven; the glass and milk pipelines mirror it
	// behind their feature flags.
	cupVision, cameraObserveSw, err := visionPickup(deps, logger, "cup",
		conf.CupVisionServiceName, conf.CameraObservePoseSwitcherName, conf.SrcCameraName)
	if err != nil {
		return nil, err
	}

	var glassVision vision.Service
	var glassObserveSw toggleswitch.Switch
	if conf.CanServeIced {
		if glassVision, glassObserveSw, err = visionPickup(deps, logger, "iced coffee glass",
			conf.GlassVisionServiceName, conf.GlassObservePoseSwitcherName, conf.SrcCameraName); err != nil {
			return nil, err
		}
	}

	var milkVision vision.Service
	var milkObserveSw toggleswitch.Switch
	if conf.CanServeIcedLatte {
		if milkVision, milkObserveSw, err = visionPickup(deps, logger, "iced latte milk",
			conf.MilkVisionServiceName, conf.MilkObservePoseSwitcherName, conf.SrcCameraName); err != nil {
			return nil, err
		}
	}

	// Speech is the one optional dependency a missing resource does not fail on:
	// the service stays usable without a voice.
	var speech resource.Resource
	if conf.SpeechServiceName != "" {
		if speechRes, ok := deps[generic.Named(conf.SpeechServiceName)]; ok {
			speech = speechRes
			logger.Infof("speech service %q connected", conf.SpeechServiceName)
		} else {
			logger.Warnf("speech service %q configured but not available", conf.SpeechServiceName)
		}
	}

	camStorage, err := optionalGenericDep(deps, logger, "cam_storage_mux_name", conf.CamStorageMuxName, "")
	if err != nil {
		return nil, err
	}
	slackNotifier, err := optionalGenericDep(deps, logger, "slack_notifier_name", conf.SlackNotifierName, "")
	if err != nil {
		return nil, err
	}
	customerDetector, err := optionalGenericDep(deps, logger, "customer_detector_name", conf.CustomerDetectorName, " — order history recording enabled")
	if err != nil {
		return nil, err
	}
	deliveryHandler, err := optionalGenericDep(deps, logger, "delivery_handler_name", conf.DeliveryHandlerName, " — peer messaging enabled")
	if err != nil {
		return nil, err
	}

	srcCamera, err := camera.FromProvider(deps, conf.SrcCameraName)
	if err != nil {
		return nil, fmt.Errorf("src_camera_name %q: %w", conf.SrcCameraName, err)
	}

	var iceBoard board.Board
	if conf.IceDispenseBoardName != "" {
		if iceBoard, err = board.FromProvider(deps, conf.IceDispenseBoardName); err != nil {
			return nil, fmt.Errorf("ice_board_name %q: %w", conf.IceDispenseBoardName, err)
		}
		logger.Infof("ice board %q connected (pin %q)", conf.IceDispenseBoardName, conf.IceDispensePinName)
	}

	var pendingOrderClipsDir string
	if conf.DataDir != "" {
		pendingOrderClipsDir = filepath.Join(conf.DataDir, "pending-clips")
		if err := os.MkdirAll(pendingOrderClipsDir, 0o755); err != nil {
			return nil, fmt.Errorf("data_dir %q: %w", conf.DataDir, err)
		}
		logger.Infof("cam storage: pending-clip records will be written to %s", pendingOrderClipsDir)
	} else {
		logger.Infof("cam storage: no data_dir configured — pending-clip records disabled (interrupted orders will not be recoverable)")
	}

	var sink orderSensorSink
	if conf.OrderSensorName != "" {
		// Same component instance as elsewhere on the robot (not a copy).
		sen, err := sensor.FromProvider(deps, conf.OrderSensorName)
		if err != nil {
			return nil, fmt.Errorf("order sensor %q: %w", conf.OrderSensorName, err)
		}
		s, ok := sen.(orderSensorSink)
		if !ok {
			return nil, fmt.Errorf("resource %q must be model viam:beanjamin:order-sensor", conf.OrderSensorName)
		}
		sink = s
		logger.Infof("order sensor %q connected", conf.OrderSensorName)
	}

	var usageSensor sensor.Sensor
	if conf.UsageSensorName != "" {
		if usageSensor, err = sensor.FromProvider(deps, conf.UsageSensorName); err != nil {
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
		deliveryHandler:      deliveryHandler,
		machineLogsURL:       buildMachineLogsURL(os.Getenv("VIAM_MACHINE_ID"), os.Getenv("VIAM_PRIMARY_ORG_ID")),
		dataLocationID:       os.Getenv("VIAM_LOCATION_ID"),
		primaryOrgID:         os.Getenv("VIAM_PRIMARY_ORG_ID"),
		pendingOrderClipsDir: pendingOrderClipsDir,
		gripper:              gripperComp,
		queue:                NewOrderQueue(),
		queueStop:            make(chan struct{}),
		orderSensorSink:      sink,
		usageSensor:          usageSensor,
		cupVision:            cupVision,
		cupCameraName:        conf.SrcCameraName,
		srcCamera:            srcCamera,
		glassVision:          glassVision,
		glassObserveSw:       glassObserveSw,
		milkVision:           milkVision,
		milkObserveSw:        milkObserveSw,
	}

	// Fail fast if the enabled configuration references poses that are missing
	// from (or unset on) the switches, rather than discovering it mid-order.
	if err := s.validateConfiguredPoses(ctx); err != nil {
		return nil, err
	}

	// Started after pose validation so a bad purge pose fails construction rather
	// than surfacing as a failed tick an hour later.
	if conf.KeepAlive != nil {
		window, err := newKeepAliveWindow(conf.KeepAlive)
		if err != nil {
			return nil, fmt.Errorf("keepalive: %w", err)
		}
		s.machineActivity = newMachineActivityStore(logger)
		go s.keepAliveLoop(window)
	}

	go s.processQueue()
	return s, nil
}

func (s *beanjaminCoffee) Name() resource.Name {
	return s.name
}

// resetCancelWaitTimeout caps how long cancel, rewind and reset_world wait for
// a running sequence to observe its cancelled context and return. Generous
// enough to cover any motion-plan cleanup; if exceeded, something is wedged and
// the operator should look at logs rather than have the command appear to
// "succeed".
const resetCancelWaitTimeout = 30 * time.Second

const cancelAnnouncement = "Stopping the current order. Nothing else will move until an operator says so."

const rewindAnnouncement = "Rewinding to a clean start. I'll clean up if needed and return to home. Click proceed when you're ready for the next order."

func (s *beanjaminCoffee) Close(context.Context) error {
	close(s.queueStop)
	s.lease.shutdown()
	// Cancelling the sequence context is not the same as closing the ice pin: a
	// rebuild or a crash mid-dispense would otherwise leave the ice machine
	// running until somebody notices.
	s.closeIcePin()
	return nil
}
