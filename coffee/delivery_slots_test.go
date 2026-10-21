package coffee

import (
	"strings"
	"testing"

	"github.com/golang/geo/r3"
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
