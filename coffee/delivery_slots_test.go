package coffee

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/rdk/vision/classification"
	"go.viam.com/rdk/vision/detection3d"

	"beanjamin/coffee/order"
)

func TestDeliverySlotPoint_OffsetsAlongTheTagsOwnY(t *testing.T) {
	for _, tc := range []struct {
		name  string
		theta float64
		want  r3.Vector
	}{
		{"tag aligned with world", 0, r3.Vector{X: 100, Y: 275, Z: 30}},
		{"tag yawed 90 degrees", 90, r3.Vector{X: 25, Y: 200, Z: 30}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tag := spatialmath.NewPose(r3.Vector{X: 100, Y: 200, Z: 30},
				&spatialmath.OrientationVectorDegrees{OZ: 1, Theta: tc.theta})
			got := deliverySlotPoint(tag, 75)
			if got.Sub(tc.want).Norm() > 1e-6 {
				t.Fatalf("deliverySlotPoint = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClosestDeliverySlot(t *testing.T) {
	slots := []deliverySlot{
		{point: r3.Vector{X: 500}, tagID: 1},
		{point: r3.Vector{X: 120}, tagID: 2},
		{point: r3.Vector{X: -300}, tagID: 3},
	}
	if got := closestDeliverySlot(slots, r3.Vector{X: 100}); got.tagID != 2 {
		t.Fatalf("closest = tag %d, want 2", got.tagID)
	}
}

func TestDetectionTagID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
		want   int
		wantOK bool
	}{
		{"apriltag id", []string{"3"}, 3, true},
		{"top classification wins", []string{"12", "4"}, 12, true},
		{"non-numeric label", []string{"tag-3"}, -1, false},
		{"no classification", nil, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			det := &detection3d.Detection{}
			for _, l := range tc.labels {
				det.Classifications = append(det.Classifications, classification.NewClassification(0.9, l))
			}
			got, ok := detectionTagID(det)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("detectionTagID = %d, %v, want %d, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestDeliveryCart_DockedForNeedsTheCartToNameTheOrder(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	running(t, c)
	id := enqueueDelivery(t, c, "espresso", "Ada", "ada@example.com")
	timeout := c.deliveryHeartbeatTimeout()

	if c.delivery.dockedFor(id, timeout) {
		t.Fatal("dockedFor before delivery_machine_ready = true, want false")
	}
	if _, err := c.deliveryMachineReady(); err != nil {
		t.Fatalf("deliveryMachineReady: %v", err)
	}
	if !c.delivery.dockedFor(id, timeout) {
		t.Fatal("dockedFor after delivery_machine_ready = false, want true")
	}
	if _, err := c.deliveryMachineHeartbeat(map[string]any{"state": "stopped"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if c.delivery.dockedFor(id, timeout) {
		t.Fatal("dockedFor on a stopped heartbeat = true, want false")
	}
}

func TestDeliveryCart_MarkHandedOverReportsTheCartSlot(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	running(t, c)
	id := enqueueDelivery(t, c, "espresso", "Ada", "ada@example.com")
	if _, err := c.deliveryMachineReady(); err != nil {
		t.Fatalf("deliveryMachineReady: %v", err)
	}

	c.delivery.markHandedOver(id, 7)
	// A fault later in the order must not undo the handoff.
	c.delivery.failIfMaking(id, "homing failed")

	got, err := c.deliveryHandoverStatus(map[string]any{"order_id": id})
	if err != nil || got["status"] != deliveryHandedOver {
		t.Fatalf("handover = %v, %v, want handed_over", got, err)
	}
	if pending, _ := c.deliveryPending(); pending["available"] != false {
		t.Fatalf("deliveryPending = %v, want nothing left to collect", pending)
	}
	c.delivery.mu.Lock()
	defer c.delivery.mu.Unlock()
	if pos := c.delivery.orders[id].snapshot()["pickup_position"]; pos != 7 {
		t.Fatalf("pickup_position = %v, want the cart slot's tag 7", pos)
	}
}

func TestDeliveryCart_ReadyForCartWaitsForThePlacement(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	running(t, c)
	id := enqueueDelivery(t, c, "espresso", "Ada", "ada@example.com")
	if _, err := c.deliveryMachineReady(); err != nil {
		t.Fatalf("deliveryMachineReady: %v", err)
	}

	c.delivery.markReadyForCart(id)
	// The cart polls while the tray is out; that must not settle the handoff
	// before the drink is in.
	got, err := c.deliveryHandoverStatus(map[string]any{"order_id": id})
	if err != nil || got["status"] != deliveryReady {
		t.Fatalf("handover while placing = %v, %v, want ready", got, err)
	}
	pending, _ := c.deliveryPending()
	if _, ok := pending["pickup_position"]; ok {
		t.Fatalf("deliveryPending = %v, want no pickup_position before the drink is set down", pending)
	}

	c.delivery.markHandedOver(id, 4)
	got, err = c.deliveryHandoverStatus(map[string]any{"order_id": id})
	if err != nil || got["status"] != deliveryHandedOver {
		t.Fatalf("handover after placement = %v, %v, want handed_over", got, err)
	}
}

func TestDeliveryCart_ReadyForCartFailsIfTheBrewFaults(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	running(t, c)
	id := enqueueDelivery(t, c, "espresso", "Ada", "ada@example.com")

	c.delivery.markReadyForCart(id)
	c.delivery.failIfMaking(id, "brew failed")

	got, err := c.deliveryHandoverStatus(map[string]any{"order_id": id})
	if err != nil || got["status"] != deliveryFailed {
		t.Fatalf("handover = %v, %v, want failed: no drink was set down", got, err)
	}
}

func TestDeliveryCart_ShelfFallbackAfterReadyForCart(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	running(t, c)
	id := enqueueDelivery(t, c, "espresso", "Ada", "ada@example.com")

	c.delivery.markReadyForCart(id)
	c.delivery.markReady(order.Order{ID: id, PickupPosition: 2}, c.deliveryHeartbeatTimeout())
	// The drink is on the shelf now, so a later fault leaves it ready.
	c.delivery.failIfMaking(id, "homing failed")

	got, err := c.deliveryHandoverStatus(map[string]any{"order_id": id})
	if err != nil || got["status"] != deliveryReady {
		t.Fatalf("handover = %v, %v, want ready on the shelf", got, err)
	}
}

func TestValidateDeliverySlots(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "unset", cfg: Config{}},
		{name: "configured", cfg: withCart(Config{})},
		{name: "switch alone", cfg: Config{DeliveryCartObservePoseSwitcherName: "sw"}, wantErr: "must be set together"},
		{name: "detector alone", cfg: Config{DeliverySlotsVisionServiceName: "vis"}, wantErr: "must be set together"},
		{name: "negative wait", cfg: Config{DeliveryCartWaitSec: -1}, wantErr: "delivery_cart_wait_sec"},
		{
			name: "missing release pose",
			cfg: Config{
				DeliveryCartObservePoseSwitcherName: "sw", DeliverySlotsVisionServiceName: "vis",
				DeliverySlotApproachRelativePose: &RelativePose{Z: 100, OZ: 1},
			},
			wantErr: "delivery_slot_release_relative_pose",
		},
		{
			name:    "release above approach",
			cfg:     withCart(Config{DeliverySlotReleaseRelativePose: &RelativePose{Z: 120, OZ: 1}}),
			wantErr: "must be below",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDeliverySlots(&tc.cfg, "coffee")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// withCart turns on direct-to-cart placement in cfg, keeping any relative pose
// it already sets.
func withCart(cfg Config) Config {
	cfg.DeliveryCartObservePoseSwitcherName = "sw"
	cfg.DeliverySlotsVisionServiceName = "vis"
	if cfg.DeliverySlotApproachRelativePose == nil {
		cfg.DeliverySlotApproachRelativePose = &RelativePose{Z: 100, OZ: 1}
	}
	if cfg.DeliverySlotReleaseRelativePose == nil {
		cfg.DeliverySlotReleaseRelativePose = &RelativePose{Z: 20, OZ: 1}
	}
	return cfg
}

// heldAtCartGrasp attaches a container of the given height to a gripper held
// level and pointing along world +Y, as it carries a drink to the cart, with
// the container's centroid centroidBelowGripMm below the grip point.
func heldAtCartGrasp(t *testing.T, heightMm, centroidBelowGripMm float64) *beanjaminCoffee {
	t.Helper()
	clawsPose := spatialmath.NewPose(r3.Vector{X: 300, Y: -100, Z: 250}, &spatialmath.OrientationVectorDegrees{OY: 1})
	fs := clawsStaticFS(t, clawsPose)
	s := heldGeomService(t, fs)
	gripWorld := spatialmath.Compose(clawsPose, spatialmath.NewPoseFromPoint(r3.Vector{Z: gripPointOffsetMm})).Point()
	worldBox, err := spatialmath.NewBox(
		spatialmath.NewPoseFromPoint(gripWorld.Sub(r3.Vector{Z: centroidBelowGripMm})),
		r3.Vector{X: 70, Y: 70, Z: heightMm}, "held")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	gripperLocal, err := geometryToWorldInverse(fs, referenceframe.NewZeroInputs(fs), worldBox)
	if err != nil {
		t.Fatalf("to gripper frame: %v", err)
	}
	addHeldItem(t, s, gripperLocal)
	s.cfg.DeliverySlotApproachRelativePose = &RelativePose{Z: 90, OZ: 1, Theta: 90}
	s.cfg.DeliverySlotReleaseRelativePose = &RelativePose{Z: -6, OZ: 1, Theta: 90}
	return s
}

// heldBottomAt returns the world Z of the bottom of the held container when
// the held-item frame stands at goal.
func heldBottomAt(t *testing.T, s *beanjaminCoffee, goal spatialmath.Pose) float64 {
	t.Helper()
	frame, box, heightMm, ok := s.scene().heldItemBox()
	if !ok {
		t.Fatalf("no held box")
	}
	framePose, err := frame.Transform(nil)
	if err != nil {
		t.Fatalf("held-item transform: %v", err)
	}
	gripAtGoal := spatialmath.Compose(goal, spatialmath.PoseInverse(framePose))
	return spatialmath.Compose(gripAtGoal, box.Pose()).Point().Z - heightMm/2
}

// A cup and a glass, held at different heights, both come to rest with their
// bottoms where the release pose's z puts them, and clear the slot by the
// approach pose's z on the way over.
func TestDeliverySlotPoses_SetTheContainerDownByItsBottom(t *testing.T) {
	slot := deliverySlot{point: r3.Vector{X: 450, Y: 300, Z: 80}, tagID: 2}
	for _, tc := range []struct {
		name                    string
		heightMm, centroidBelow float64
		wantDepthBelowGripMm    float64
	}{
		{name: "espresso cup", heightMm: 60, centroidBelow: 6, wantDepthBelowGripMm: 36},
		{name: "iced glass", heightMm: 165, centroidBelow: 0, wantDepthBelowGripMm: 82.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := heldAtCartGrasp(t, tc.heightMm, tc.centroidBelow)
			depth, ok := s.scene().heldItemBottomDepthMm()
			if !ok || math.Abs(depth-tc.wantDepthBelowGripMm) > 1e-6 {
				t.Fatalf("bottom depth = %v (ok=%v), want %v", depth, ok, tc.wantDepthBelowGripMm)
			}

			approach, release, retreat, err := s.deliverySlotPoses(slot)
			if err != nil {
				t.Fatalf("deliverySlotPoses: %v", err)
			}
			for _, pd := range []*poseData{approach, release} {
				if pd.componentName != heldItemFrameName {
					t.Fatalf("goal moves %q, want the held-item frame", pd.componentName)
				}
			}
			if got := heldBottomAt(t, s, release.pose); math.Abs(got-(slot.point.Z-6)) > 1e-6 {
				t.Errorf("bottom at release Z = %v, want %v", got, slot.point.Z-6)
			}
			if got := heldBottomAt(t, s, approach.pose); math.Abs(got-(slot.point.Z+90)) > 1e-6 {
				t.Errorf("bottom at approach Z = %v, want %v", got, slot.point.Z+90)
			}
			requireVecEqual(t, release.pose.Point(), r3.Vector{X: slot.point.X, Y: slot.point.Y, Z: slot.point.Z - 6 + depth}, 1e-6)

			// The retreat is the gripper pose that held the container at the approach.
			if retreat.componentName != gripPoint {
				t.Fatalf("retreat moves %q, want %q", retreat.componentName, gripPoint)
			}
			frame, _, _, _ := s.scene().heldItemBox()
			framePose, err := frame.Transform(nil)
			if err != nil {
				t.Fatalf("held-item transform: %v", err)
			}
			if !spatialmath.PoseAlmostEqual(spatialmath.Compose(retreat.pose, framePose), approach.pose) {
				t.Errorf("held item at retreat = %v, want the approach %v", spatialmath.Compose(retreat.pose, framePose), approach.pose)
			}
			// theta 90 is the gripper pointing along world +Y, level.
			toolAxis := spatialmath.Compose(retreat.pose, spatialmath.NewPoseFromPoint(r3.Vector{Z: 1})).Point().Sub(retreat.pose.Point())
			requireVecEqual(t, toolAxis, r3.Vector{Y: 1}, 1e-6)
		})
	}
}

func TestDeliverySlotPoses_NothingHeldFallsBackToTheShelf(t *testing.T) {
	s := heldGeomService(t, clawsStaticFS(t, spatialmath.NewZeroPose()))
	_, _, _, err := s.deliverySlotPoses(deliverySlot{})
	if !errors.Is(err, errMotionPlanning) {
		t.Fatalf("err = %v, want it to wrap errMotionPlanning", err)
	}
}

func TestDeliveryStandIn(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{}}
	for _, arg := range []any{true, "cup"} {
		if _, label, err := s.deliveryStandIn(arg); err != nil || label != pickupLabelCup {
			t.Errorf("deliveryStandIn(%v) = %q, %v; want the cup", arg, label, err)
		}
	}
	if _, _, err := s.deliveryStandIn("glass"); err == nil || !strings.Contains(err.Error(), "glass_dimensions") {
		t.Errorf("glass without glass config: err = %v, want it to name glass_dimensions", err)
	}
	s.cfg.GlassDimensions = &ContainerDimensions{DiameterMm: 78, HeightMm: 165}
	s.cfg.GlassGrabRelativePose = &RelativePose{OY: -1, Y: -15}
	if _, label, err := s.deliveryStandIn("glass"); err != nil || label != pickupLabelGlass {
		t.Errorf("deliveryStandIn(glass) = %q, %v; want the glass", label, err)
	}
	for _, arg := range []any{false, "mug", nil} {
		if _, _, err := s.deliveryStandIn(arg); err == nil {
			t.Errorf("deliveryStandIn(%v): want an error", arg)
		}
	}
}
