package beanjamin

import (
	"testing"

	"go.viam.com/rdk/logging"
)

// TestActiveOrderLogger_FallbackAndHandoff covers the contract every de-threaded
// helper — and, critically, cancel() — relies on: activeOrderLogger() returns
// the base logger when no order is in flight, the published order-scoped logger
// while one is, and falls back to the base logger once processQueue clears it.
func TestActiveOrderLogger_FallbackAndHandoff(t *testing.T) {
	s, _ := newTestCoffee(t, nil)

	// Idle: no order in flight → base logger.
	if got := s.activeOrderLogger(); got != s.logger {
		t.Fatal("idle activeOrderLogger() should return the base logger")
	}

	// Active: processQueue publishes a tagged logger → handoff returns it.
	orderLogger := s.logger.WithFields("order_id", "order-abc")
	s.activeLogger.Store(&orderLogger)
	if got := s.activeOrderLogger(); got != orderLogger {
		t.Fatal("active activeOrderLogger() should return the published order logger")
	}

	// Cleared: processQueue stores nil after the order → fall back to base.
	s.activeLogger.Store(nil)
	if got := s.activeOrderLogger(); got != s.logger {
		t.Fatal("after clearing, activeOrderLogger() should return the base logger again")
	}
}

// TestActiveOrderLogger_TagsOrderID verifies the whole point of the feature:
// a log emitted through activeOrderLogger() while an order is active actually
// carries the order_id field, so logs are queryable by order.
func TestActiveOrderLogger_TagsOrderID(t *testing.T) {
	base, observed := logging.NewObservedTestLogger(t)
	s := &beanjaminCoffee{logger: base}

	orderLogger := base.WithFields("order_id", "order-xyz")
	s.activeLogger.Store(&orderLogger)

	s.activeOrderLogger().Infof("brewing step")

	for _, e := range observed.All() {
		if e.Message != "brewing step" {
			continue
		}
		if v, ok := e.ContextMap()["order_id"]; ok && v == "order-xyz" {
			return // tagged as expected
		}
		t.Fatalf("log entry %q missing order_id field: context=%v", e.Message, e.ContextMap())
	}
	t.Fatalf("expected a log entry %q, got %+v", "brewing step", observed.All())
}
