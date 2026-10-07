package coffee

import (
	"context"
	"sync"
	"sync/atomic"
)

// armLease groups the state that decides who may move the arm, how to
// interrupt them, and whether the queue may start the next order:
//
//   - running is the gate. A sequence that moves the arm takes it with claim,
//     which also hands back cancelCtx, and releases it with Store(false).
//     proceed and reset_world, which never watch cancelCtx, still take it with
//     running.CompareAndSwap(false, true).
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

// claim takes the arm and returns the cancelCtx the caller's motion must watch,
// reporting false when another sequence already holds it. Both happen under mu,
// the same lock signalCancel holds while it checks running and replaces
// cancelCtx, so a cancel lands either before the claim (and finds nothing to
// stop) or after it (and cancels exactly the context this caller got). Release
// with running.Store(false).
func (l *armLease) claim() (context.Context, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running.CompareAndSwap(false, true) {
		return nil, false
	}
	return l.cancelCtx, true
}
