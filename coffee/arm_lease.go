package coffee

import (
	"context"
	"sync"
	"sync/atomic"
)

// armLease groups the state that decides who may move the arm, how to
// interrupt them, and whether the queue may start the next order. The fields
// and their rules are unchanged from when they sat directly on beanjaminCoffee:
//
//   - running is the gate. A sequence claims the arm with
//     running.CompareAndSwap(false, true) and releases it with Store(false).
//   - cancelCtx is the shared context motion steps watch, guarded by mu.
//     signalCancel cancels it through cancelFunc and replaces both.
//   - paused holds the queue back after a cancel or a fault until proceed.
type armLease struct {
	mu         sync.Mutex
	cancelCtx  context.Context
	cancelFunc func()
	running    atomic.Bool
	paused     atomic.Bool
}
