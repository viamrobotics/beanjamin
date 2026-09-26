package speech

// When an order genuinely faults (not an operator cancel — cancels speak
// their own calmer announcement), the machine speaks a snarky failure line
// and raises a transient fault_active flag in the coffee service's Status(),
// which the conversation-bundle stack (voice-command → led-bridge) renders as
// a red LED flash. This package knows nothing about LEDs.

import (
	"context"
	"sync/atomic"
	"time"

	"beanjamin/coffee/order"

	"go.viam.com/rdk/logging"
)

// faultWindow is how long fault_active stays raised after a genuine fault —
// the window the LED strip flashes red for. A var so tests can shorten it.
var faultWindow = 5 * time.Second

// FaultAlarm owns the transient fault_active flag and the spoken failure line
// for genuinely faulted orders. A nil *FaultAlarm is never active and ignores
// every reading.
type FaultAlarm struct {
	speaker *Speaker
	logger  logging.Logger
	active  atomic.Bool
}

// NewFaultAlarm returns an alarm that speaks through speaker and logs speech
// failures on logger.
func NewFaultAlarm(speaker *Speaker, logger logging.Logger) *FaultAlarm {
	return &FaultAlarm{speaker: speaker, logger: logger}
}

// Active reports whether the alarm is within faultWindow of a genuine fault;
// the coffee service surfaces it in Status() as fault_active.
func (a *FaultAlarm) Active() bool {
	return a != nil && a.active.Load()
}

// ReactToOrderFailure raises the transient fault window and speaks a snarky
// failure line for a genuine fault. It is a no-op for successful orders and
// operator cancels, and the speech is best-effort off the caller's goroutine.
func (a *FaultAlarm) ReactToOrderFailure(r order.Reading) {
	if a == nil || r.ExecErr == nil || r.OperatorCancelled {
		return
	}
	a.active.Store(true)
	// Overlapping faults within faultWindow may clear the flag early; a rare
	// double-fault clearing the LED a few seconds sooner isn't worth guarding.
	time.AfterFunc(faultWindow, func() { a.active.Store(false) })
	if !a.speaker.Configured() {
		return
	}
	line := OrderFailed(r.Order.Drink, r.Order.DisplayName())
	logger := a.logger.WithFields("order_id", r.Order.ID)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.speaker.SayAlways(ctx, line); err != nil {
			logger.Warnf("failed to say failure line: %v", err)
		}
	}()
}
