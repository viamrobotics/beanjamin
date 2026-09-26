package coffee

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"

	"beanjamin/coffee/order"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// fakeSpeech records say_async calls dispatched through s.say so tests can
// assert how many announcements enqueueOrder produced.
type fakeSpeech struct {
	resource.AlwaysRebuild
	mu   sync.Mutex
	said []string
}

func (f *fakeSpeech) Name() resource.Name { return resource.Name{} }
func (f *fakeSpeech) DoCommand(_ context.Context, cmd map[string]any) (map[string]any, error) {
	if t, ok := cmd["say_async"].(string); ok {
		f.mu.Lock()
		f.said = append(f.said, t)
		f.mu.Unlock()
	}
	return map[string]any{}, nil
}
func (f *fakeSpeech) Close(_ context.Context) error { return nil }
func (f *fakeSpeech) Status(_ context.Context) (map[string]any, error) {
	return map[string]any{}, nil
}
func (f *fakeSpeech) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.said))
	copy(out, f.said)
	return out
}

// newTestCoffee builds a minimal *beanjaminCoffee suitable for exercising
// enqueueOrder. Real motion/arm wiring is intentionally absent — only the
// fields enqueueOrder touches are populated.
func newTestCoffee(t *testing.T, cfg *Config) (*beanjaminCoffee, *fakeSpeech) {
	t.Helper()
	if cfg == nil {
		cfg = &Config{CanServeDecaf: true}
	}
	speech := &fakeSpeech{}
	return &beanjaminCoffee{
		logger: logging.NewTestLogger(t),
		cfg:    cfg,
		queue:  order.NewQueue(),
		speech: speech,
	}, speech
}

func TestEnqueueOrder_DefaultsCountToOne(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	resp, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink":         "espresso",
		"customer_name": "Alice",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := resp["count"].(int); got != 1 {
		t.Errorf("count = %v, want 1", resp["count"])
	}
	ids, _ := resp["order_ids"].([]string)
	if len(ids) != 1 {
		t.Fatalf("order_ids length = %d, want 1", len(ids))
	}
	if id, _ := resp["order_id"].(string); id != ids[0] {
		t.Errorf("order_id %q must match order_ids[0] %q", id, ids[0])
	}
	if c.queue.Len() != 1 {
		t.Errorf("queue length = %d, want 1", c.queue.Len())
	}
}

func TestEnqueueOrder_BatchEnqueuesN(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	resp, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink":         "espresso",
		"customer_name": "Esha",
		"count":         float64(3),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := resp["count"].(int); got != 3 {
		t.Errorf("count = %v, want 3", resp["count"])
	}
	ids, _ := resp["order_ids"].([]string)
	if len(ids) != 3 {
		t.Fatalf("order_ids length = %d, want 3", len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			t.Errorf("empty UUID in order_ids")
		}
		if seen[id] {
			t.Errorf("duplicate UUID %q in order_ids", id)
		}
		seen[id] = true
	}
	if c.queue.Len() != 3 {
		t.Errorf("queue length = %d, want 3", c.queue.Len())
	}
	if pos, _ := resp["queue_position"].(int); pos != 1 {
		t.Errorf("queue_position = %v, want 1 (first order)", resp["queue_position"])
	}
}

func TestEnqueueOrder_Fulfillment(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"absent defaults to pickup", nil, order.FulfillmentPickup},
		{"empty defaults to pickup", "", order.FulfillmentPickup},
		{"pickup", "pickup", order.FulfillmentPickup},
		{"delivery", "delivery", order.FulfillmentDelivery},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestCoffee(t, nil)
			payload := map[string]any{
				"drink":         "espresso",
				"customer_name": "Alice",
				// Required for the delivery case; harmless for the others.
				"customer_email": "alice@example.com",
			}
			if tc.v != nil {
				payload["fulfillment"] = tc.v
			}
			if _, err := c.enqueueOrder(context.Background(), payload); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			orders := c.queue.List()
			if len(orders) != 1 {
				t.Fatalf("queue length = %d, want 1", len(orders))
			}
			if o := orders[0]; o.Fulfillment != tc.want {
				t.Errorf("Fulfillment = %q, want %q", o.Fulfillment, tc.want)
			}
		})
	}
}

func TestEnqueueOrder_DeliveryRequiresEmail(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	_, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink":         "espresso",
		"customer_name": "Alice",
		"fulfillment":   "delivery",
	})
	if err == nil {
		t.Fatal("expected error for delivery order without customer_email, got nil")
	}
	if c.queue.Len() != 0 {
		t.Errorf("queue should stay empty after rejection, got len=%d", c.queue.Len())
	}

	resp, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink":          "espresso",
		"customer_name":  "Alice",
		"customer_email": "alice@example.com",
		"fulfillment":    "delivery",
	})
	if err != nil {
		t.Fatalf("unexpected error with email present: %v", err)
	}
	if resp["status"] != "queued" {
		t.Errorf("status = %v, want queued", resp["status"])
	}
	orders := c.queue.List()
	if len(orders) != 1 || orders[0].Fulfillment != order.FulfillmentDelivery || orders[0].CustomerEmail != "alice@example.com" {
		t.Errorf("queued orders = %+v, want one delivery with email", orders)
	}
}

func TestEnqueueOrder_RejectsBadFulfillment(t *testing.T) {
	cases := []struct {
		name string
		v    any
	}{
		{"unknown value", "drone"},
		{"non-string", float64(1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestCoffee(t, nil)
			_, err := c.enqueueOrder(context.Background(), map[string]any{
				"drink":       "espresso",
				"fulfillment": tc.v,
			})
			if err == nil {
				t.Fatalf("expected error for fulfillment=%v, got nil", tc.v)
			}
			if c.queue.Len() != 0 {
				t.Errorf("queue should stay empty after rejection, got len=%d", c.queue.Len())
			}
		})
	}
}

func TestEnqueueOrder_RejectsBadCount(t *testing.T) {
	cases := []struct {
		name string
		v    any
	}{
		{"zero", float64(0)},
		{"negative", float64(-1)},
		{"fractional", 1.5},
		{"string", "5"},
		{"nan", math.NaN()},
		{"inf", math.Inf(1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestCoffee(t, nil)
			_, err := c.enqueueOrder(context.Background(), map[string]any{
				"drink": "espresso",
				"count": tc.v,
			})
			if err == nil {
				t.Fatalf("expected error for count=%v, got nil", tc.v)
			}
			if c.queue.Len() != 0 {
				t.Errorf("queue should stay empty after rejection, got len=%d", c.queue.Len())
			}
		})
	}
}

func TestEnqueueOrder_RejectsAboveCap(t *testing.T) {
	c, _ := newTestCoffee(t, &Config{CanServeDecaf: true, MaxBatchSize: 5})
	if _, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink": "espresso",
		"count": float64(5),
	}); err != nil {
		t.Fatalf("count=cap should be allowed; got error: %v", err)
	}
	if c.queue.Len() != 5 {
		t.Errorf("queue length after at-cap batch = %d, want 5", c.queue.Len())
	}

	c2, _ := newTestCoffee(t, &Config{CanServeDecaf: true, MaxBatchSize: 5})
	if _, err := c2.enqueueOrder(context.Background(), map[string]any{
		"drink": "espresso",
		"count": float64(6),
	}); err == nil {
		t.Fatal("count=cap+1 should be rejected; got nil error")
	}
	if c2.queue.Len() != 0 {
		t.Errorf("queue should stay empty after over-cap rejection, got len=%d", c2.queue.Len())
	}
}

func TestEnqueueOrder_BatchSuppressesPerOrderAnnouncement(t *testing.T) {
	c, sp := newTestCoffee(t, &Config{CanServeDecaf: true, Conversational: true})
	// Pre-populate the queue so the single-order path would normally fire
	// pickOrderReceived (pos > 1). The batch path should fire exactly one
	// pickOrderReceivedBatch line instead.
	c.queue.Enqueue(order.NewOrder("lungo", "Bob", "", ""))

	if _, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink":         "espresso",
		"customer_name": "Esha",
		"count":         float64(5),
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	calls := sp.calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 speech call for batch, got %d: %v", len(calls), calls)
	}
	// The consolidated batch line mentions the count and the (plural) drink.
	if !strings.Contains(calls[0], "5") || !strings.Contains(calls[0], "espressos") {
		t.Errorf("batch line %q should reference count=5 and 'espressos'", calls[0])
	}
}

func TestEnqueueOrder_RejectsBatchOfUnsupportedDrink(t *testing.T) {
	c, _ := newTestCoffee(t, &Config{CanServeDecaf: false})
	if _, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink": "decaf",
		"count": float64(3),
	}); err == nil {
		t.Fatal("expected rejection for decaf when can_serve_decaf=false")
	}
	if c.queue.Len() != 0 {
		t.Errorf("queue should stay empty after pre-loop rejection, got len=%d", c.queue.Len())
	}
}

func TestEnqueueOrder_IcedGatedByCanServeIced(t *testing.T) {
	// Rejected when can_serve_iced is off.
	c, _ := newTestCoffee(t, &Config{})
	if _, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink": "iced_coffee",
	}); err == nil {
		t.Fatal("expected rejection for iced_coffee when can_serve_iced=false")
	}
	if c.queue.Len() != 0 {
		t.Errorf("queue should stay empty after rejection, got len=%d", c.queue.Len())
	}

	// Accepted when can_serve_iced is on.
	c2, _ := newTestCoffee(t, &Config{CanServeIced: true})
	if _, err := c2.enqueueOrder(context.Background(), map[string]any{
		"drink": "iced_coffee",
	}); err != nil {
		t.Fatalf("unexpected error enqueuing iced_coffee with can_serve_iced=true: %v", err)
	}
	if c2.queue.Len() != 1 {
		t.Errorf("queue length = %d, want 1", c2.queue.Len())
	}
}

func TestEnqueueOrder_IcedLatteGatedByCanServeIcedLatte(t *testing.T) {
	// Rejected on a machine that serves iced coffee but has no milk configured —
	// can_serve_iced alone doesn't buy the fridge trip.
	c, _ := newTestCoffee(t, &Config{CanServeIced: true})
	if _, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink": "iced_latte",
	}); err == nil {
		t.Fatal("expected rejection for iced_latte when can_serve_iced_latte=false")
	}
	if c.queue.Len() != 0 {
		t.Errorf("queue should stay empty after rejection, got len=%d", c.queue.Len())
	}

	c2, _ := newTestCoffee(t, &Config{CanServeIced: true, CanServeIcedLatte: true})
	if _, err := c2.enqueueOrder(context.Background(), map[string]any{
		"drink": "iced_latte",
	}); err != nil {
		t.Fatalf("unexpected error enqueuing iced_latte with can_serve_iced_latte=true: %v", err)
	}
	if c2.queue.Len() != 1 {
		t.Errorf("queue length = %d, want 1", c2.queue.Len())
	}
}

func TestEnqueueOrder_CarriesBothNames(t *testing.T) {
	tests := []struct {
		name         string
		payload      map[string]any
		wantName     string
		wantModified string
	}{
		{
			name: "kiosk sends the real name and the misspelling it displayed",
			payload: map[string]any{
				"drink":                  "espresso",
				"customer_name":          "Vijay",
				"modified_customer_name": "Vijoy",
			},
			wantName:     "Vijay",
			wantModified: "Vijoy",
		},
		{
			name: "surrounding whitespace is trimmed off the tracking name",
			payload: map[string]any{
				"drink":                  "espresso",
				"customer_name":          "  Vijay  ",
				"modified_customer_name": "Vijoy",
			},
			wantName:     "Vijay",
			wantModified: "Vijoy",
		},
		{
			name: "caller that never misspells sends only the real name",
			payload: map[string]any{
				"drink":         "espresso",
				"customer_name": "Ada",
			},
			wantName:     "Ada",
			wantModified: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestCoffee(t, nil)
			if _, err := c.enqueueOrder(context.Background(), tc.payload); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			orders := c.queue.List()
			if len(orders) != 1 {
				t.Fatalf("queue length = %d, want 1", len(orders))
			}
			if got := orders[0].CustomerName; got != tc.wantName {
				t.Errorf("CustomerName = %q, want %q", got, tc.wantName)
			}
			if got := orders[0].ModifiedCustomerName; got != tc.wantModified {
				t.Errorf("ModifiedCustomerName = %q, want %q", got, tc.wantModified)
			}
		})
	}
}
