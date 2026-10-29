package coffee

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"beanjamin/coffee/order"
	"beanjamin/coffee/speech"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// fakeSpeech records say_async calls dispatched through s.speaker so tests can
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
		cfg = &Config{BeanOptions: []BeanOption{{Name: "decaf"}}}
	}
	sp := &fakeSpeech{}
	return &beanjaminCoffee{
		logger:   logging.NewTestLogger(t),
		cfg:      cfg,
		queue:    order.NewQueue(),
		speaker:  speech.NewSpeaker(sp, cfg.Conversational),
		delivery: newDeliveryCart(),
	}, sp
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
			if tc.want == order.FulfillmentDelivery {
				if _, err := c.deliveryMachineHeartbeat(map[string]any{"state": "running"}); err != nil {
					t.Fatalf("heartbeat: %v", err)
				}
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

	if _, err := c.deliveryMachineHeartbeat(map[string]any{"state": "running"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
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
	c, _ := newTestCoffee(t, &Config{BeanOptions: []BeanOption{{Name: "decaf"}}, MaxBatchSize: 5})
	if _, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink": "espresso",
		"count": float64(5),
	}); err != nil {
		t.Fatalf("count=cap should be allowed; got error: %v", err)
	}
	if c.queue.Len() != 5 {
		t.Errorf("queue length after at-cap batch = %d, want 5", c.queue.Len())
	}

	c2, _ := newTestCoffee(t, &Config{BeanOptions: []BeanOption{{Name: "decaf"}}, MaxBatchSize: 5})
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
	c, sp := newTestCoffee(t, &Config{BeanOptions: []BeanOption{{Name: "decaf"}}, Conversational: true})
	// Pre-populate the queue so the single-order path would normally fire
	// speech.OrderReceived (pos > 1). The batch path should fire exactly one
	// speech.OrderReceivedBatch line instead.
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
	c, _ := newTestCoffee(t, &Config{})
	if _, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink": "mocha",
		"count": float64(3),
	}); err == nil {
		t.Fatal("expected rejection for an unknown drink")
	}
	if c.queue.Len() != 0 {
		t.Errorf("queue should stay empty after pre-loop rejection, got len=%d", c.queue.Len())
	}
}

// Clients that predate beans still order decaf by drink id ("decaf",
// "decaf_lungo"): those are taken as espresso or lungo with decaf beans, on a
// machine with a decaf grinder, whether configured the new or the old way.
func TestEnqueueOrder_LegacyDecafDrinks(t *testing.T) {
	for name, cfg := range map[string]*Config{
		"bean_options":    {BeanOptions: []BeanOption{{Name: "decaf"}}},
		"can_serve_decaf": {CanServeDecaf: true},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newTestCoffee(t, cfg)
			for _, req := range []map[string]any{
				{"drink": "decaf"},
				{"drink": "decaf_lungo"},
				{"drink": "decaf", "beans": "decaf"}, // redundant but consistent
			} {
				if _, err := c.enqueueOrder(context.Background(), req); err != nil {
					t.Fatalf("%v: %v", req, err)
				}
			}
			var got []string
			for _, o := range c.queue.List() {
				got = append(got, o.Drink+"/"+o.Beans)
			}
			if want := "espresso/decaf,lungo/decaf,espresso/decaf"; strings.Join(got, ",") != want {
				t.Errorf("queued %v, want %s", got, want)
			}
		})
	}

	// A legacy id that contradicts an explicit beans is refused.
	c, _ := newTestCoffee(t, &Config{BeanOptions: []BeanOption{{Name: "decaf"}, {Name: "light_roast"}}})
	if _, err := c.enqueueOrder(context.Background(), map[string]any{"drink": "decaf", "beans": "light_roast"}); err == nil {
		t.Error("decaf with light_roast beans should be rejected")
	}
	// And decaf still needs a decaf grinder.
	plain, _ := newTestCoffee(t, &Config{})
	if _, err := plain.enqueueOrder(context.Background(), map[string]any{"drink": "decaf"}); err == nil {
		t.Error("decaf should be rejected without a decaf grinder")
	}
	if c.queue.Len()+plain.queue.Len() != 0 {
		t.Error("rejected orders must not be queued")
	}
}

// The beans an order names are validated against bean_options and land on
// every order of the batch; naming none gets the regular beans.
func TestEnqueueOrder_Beans(t *testing.T) {
	c, _ := newTestCoffee(t, &Config{
		CanServeIced: true,
		BeanOptions:  []BeanOption{{Name: "decaf"}, {Name: "light_roast"}},
	})
	if _, err := c.enqueueOrder(context.Background(), map[string]any{"drink": "espresso"}); err != nil {
		t.Fatalf("plain espresso: %v", err)
	}
	// Any drink takes any configured beans, iced ones included.
	if _, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink": "iced_coffee", "beans": "light_roast", "count": float64(2),
	}); err != nil {
		t.Fatalf("light roast iced coffee: %v", err)
	}
	if _, err := c.enqueueOrder(context.Background(), map[string]any{"drink": "lungo", "beans": "decaf"}); err != nil {
		t.Fatalf("decaf lungo: %v", err)
	}
	var got []string
	for _, o := range c.queue.List() {
		got = append(got, o.Drink+"/"+o.Beans)
	}
	want := []string{"espresso/regular", "iced_coffee/light_roast", "iced_coffee/light_roast", "lungo/decaf"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("queued = %v, want %v", got, want)
	}

	for _, bad := range []any{"dark_roast", float64(1)} {
		if _, err := c.enqueueOrder(context.Background(), map[string]any{"drink": "espresso", "beans": bad}); err == nil {
			t.Errorf("beans %#v should be rejected", bad)
		}
	}
	if c.queue.Len() != len(want) {
		t.Errorf("rejected orders must not be queued, len=%d", c.queue.Len())
	}

	plain, _ := newTestCoffee(t, &Config{})
	if _, err := plain.enqueueOrder(context.Background(), map[string]any{"drink": "espresso", "beans": "decaf"}); err == nil {
		t.Error("decaf should be rejected on a machine with no bean_options")
	}
}

// Speech names the beans of a non-regular order.
func TestEnqueueOrder_SpeaksBeans(t *testing.T) {
	c, sp := newTestCoffee(t, &Config{Conversational: true, BeanOptions: []BeanOption{{Name: "light_roast"}}})
	c.queue.Enqueue(order.NewOrder("lungo", "Bob", "", "")) // so the order-received line fires
	if _, err := c.enqueueOrder(context.Background(), map[string]any{"drink": "lungo", "beans": "light_roast"}); err != nil {
		t.Fatal(err)
	}
	calls := sp.calls()
	if len(calls) != 1 || !strings.Contains(calls[0], "light roast lungo") {
		t.Errorf("speech = %v, want a line naming the light roast lungo", calls)
	}
	if g := c.queue.List()[1].Greeting; !strings.Contains(g, "light roast lungo") {
		t.Errorf("greeting = %q, want it to name the light roast lungo", g)
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

// TestWaitForArmHoldsTheQueueUntilProceed: a paused queue does not claim the
// arm, and the pause stays set while it waits, so a proceed arriving at any
// moment sees a paused queue and grants the resume.
func TestWaitForArmHoldsTheQueueUntilProceed(t *testing.T) {
	s, _, _ := pausedCoffeeWithDirtyWorld(t, nil)
	s.queueStop = make(chan struct{})
	t.Cleanup(func() { close(s.queueStop) })

	claimed := make(chan bool, 1)
	go func() {
		_, ok := s.waitForArm()
		claimed <- ok
	}()

	select {
	case <-claimed:
		t.Fatal("waitForArm claimed the arm while the queue was still paused")
	case <-time.After(3 * queueArmPollInterval):
	}
	if !s.lease.isPaused() {
		t.Fatal("paused must stay set while the queue waits")
	}

	if _, err := s.proceedQueue(context.Background()); err != nil {
		t.Fatalf("proceed error: %v", err)
	}
	select {
	case ok := <-claimed:
		if !ok {
			t.Fatal("waitForArm reported shutdown, want a claim")
		}
	case <-time.After(time.Second):
		t.Fatal("waitForArm never claimed the arm after proceed")
	}
	if got := s.lease.holderName(); got != "order queue" {
		t.Errorf("holder = %q, want the order queue", got)
	}
	s.lease.release()
}

// TestWaitForArmWaitsOutABusyArm: an order queued behind another holder, such
// as a keepalive purge, waits for the arm instead of failing as a fault.
func TestWaitForArmWaitsOutABusyArm(t *testing.T) {
	s, _, _ := coffeeWithDirtyWorld(t, nil)
	s.queueStop = make(chan struct{})
	mustClaim(t, s, "keepalive purge")

	claimed := make(chan bool, 1)
	go func() {
		_, ok := s.waitForArm()
		claimed <- ok
	}()

	select {
	case <-claimed:
		t.Fatal("waitForArm claimed an arm someone else holds")
	case <-time.After(3 * queueArmPollInterval):
	}
	if s.lease.isPaused() {
		t.Error("waiting for a busy arm must not pause the queue")
	}

	s.lease.release()
	select {
	case ok := <-claimed:
		if !ok {
			t.Fatal("waitForArm reported shutdown, want a claim")
		}
	case <-time.After(time.Second):
		t.Fatal("waitForArm never claimed the arm after it was released")
	}
	s.lease.release()
	close(s.queueStop)
}

// TestWaitForArmStopsOnShutdown: a waiting queue gives up once the service
// closes.
func TestWaitForArmStopsOnShutdown(t *testing.T) {
	s, _, _ := pausedCoffeeWithDirtyWorld(t, nil)
	s.queueStop = make(chan struct{})

	claimed := make(chan bool, 1)
	go func() {
		_, ok := s.waitForArm()
		claimed <- ok
	}()
	close(s.queueStop)

	select {
	case ok := <-claimed:
		if ok {
			t.Error("waitForArm should report shutdown once queueStop closes")
		}
	case <-time.After(time.Second):
		t.Fatal("waitForArm never returned after queueStop closed")
	}
}
