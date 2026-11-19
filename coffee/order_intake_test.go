package coffee

import (
	"context"
	"strings"
	"testing"
)

func TestEnqueueOrder_RejectsWrongTypedFieldBeforeQueueing(t *testing.T) {
	c, sp := newTestCoffee(t, nil)
	_, err := c.enqueueOrder(context.Background(), map[string]any{
		"drink":          "espresso",
		"customer_name":  "Alice",
		"customer_email": float64(42),
	})
	if err == nil || !strings.Contains(err.Error(), "customer_email") {
		t.Fatalf("error = %v, want one naming customer_email", err)
	}
	if c.queue.Len() != 0 {
		t.Errorf("queue should stay empty after rejection, got len=%d", c.queue.Len())
	}
	if calls := sp.calls(); len(calls) != 0 {
		t.Errorf("a malformed payload should not be spoken to, got %v", calls)
	}
}

func TestEnqueueRejectsMugDrinkWhenOff(t *testing.T) {
	c, _ := newTestCoffee(t, nil)
	_, err := c.enqueueOrder(context.Background(), map[string]any{"drink": "americano", "customer_name": "Alice"})
	if err == nil || !strings.Contains(err.Error(), "can_serve_mug_drinks=false") {
		t.Fatalf("error = %v, want one naming can_serve_mug_drinks=false", err)
	}
	if c.queue.Len() != 0 {
		t.Errorf("queue len = %d, want 0", c.queue.Len())
	}

	c, _ = newTestCoffee(t, &Config{CanServeMugDrinks: true})
	if _, err := c.enqueueOrder(context.Background(), map[string]any{"drink": "americano", "customer_name": "Alice"}); err != nil {
		t.Fatalf("americano with flag on: %v", err)
	}
	if c.queue.Len() != 1 {
		t.Errorf("queue len = %d, want 1", c.queue.Len())
	}
}

func TestEnqueueRejectsDeliveryForMugDrinks(t *testing.T) {
	for _, drink := range []string{"americano", "hot_water"} {
		c, _ := newTestCoffee(t, &Config{CanServeMugDrinks: true})
		_, err := c.enqueueOrder(context.Background(), map[string]any{
			"drink": drink, "customer_name": "Alice", "customer_email": "alice@example.com", "fulfillment": "delivery",
		})
		if err == nil || !strings.Contains(err.Error(), "delivery isn't available for") {
			t.Fatalf("%s delivery: error = %v, want a delivery rejection", drink, err)
		}
		if c.queue.Len() != 0 {
			t.Errorf("%s delivery: queue len = %d, want 0", drink, c.queue.Len())
		}

		if _, err := c.enqueueOrder(context.Background(), map[string]any{
			"drink": drink, "customer_name": "Alice", "fulfillment": "pickup",
		}); err != nil {
			t.Fatalf("%s pickup: %v", drink, err)
		}
		if c.queue.Len() != 1 {
			t.Errorf("%s pickup: queue len = %d, want 1", drink, c.queue.Len())
		}
	}
}
