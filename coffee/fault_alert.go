package coffee

// When an order genuinely faults (not an operator cancel — cancels speak
// their own calmer announcement from control.go), the machine speaks a snarky
// failure line and raises a transient fault_active flag in Status(), which
// the conversation-bundle stack (voice-command → led-bridge) renders as a red
// LED flash. This service knows nothing about LEDs.

import (
	"context"
	"time"
)

// faultWindow is how long fault_active stays raised after a genuine fault —
// the window the LED strip flashes red for. A var so tests can shorten it.
var faultWindow = 5 * time.Second

// reactToOrderFailure raises the transient fault window and speaks a snarky
// failure line for a genuine fault. It is a no-op for successful orders and
// operator cancels, and the speech is best-effort off the queue goroutine.
func (s *beanjaminCoffee) reactToOrderFailure(r orderReading) {
	if r.execErr == nil || r.operatorCancelled {
		return
	}
	s.faultActive.Store(true)
	// Overlapping faults within faultWindow may clear the flag early; a rare
	// double-fault clearing the LED a few seconds sooner isn't worth guarding.
	time.AfterFunc(faultWindow, func() { s.faultActive.Store(false) })
	if s.speech == nil {
		return
	}
	line := pickOrderFailed(r.order.Drink, r.order.CustomerName)
	logger := s.logger.WithFields("order_id", r.order.ID)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.sayAlways(ctx, line); err != nil {
			logger.Warnf("failed to say failure line: %v", err)
		}
	}()
}
