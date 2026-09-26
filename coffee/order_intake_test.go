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
