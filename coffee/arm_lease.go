package coffee

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// armLease groups the state that decides who may move the arm, how to
// interrupt them, and whether the queue may start the next order:
//
//   - holder names who has the arm, "" when it is free. Every sequence that
//     moves the arm or swaps the frame system takes it with claim, which also
//     hands back cancelCtx, and gives it back with release. It is guarded by
//     mu, the same lock as cancelCtx.
//   - cancelCtx is the shared context motion steps watch, guarded by mu.
//     signalCancel cancels it through cancelFunc and replaces both.
//   - paused holds the queue back after a cancel or a fault until proceed.
type armLease struct {
	mu         sync.Mutex
	cancelCtx  context.Context
	cancelFunc func()
	holder     string // who holds the arm, for logs, errors and Status; "" when free
	paused     atomic.Bool
}

// errArmBusy is what claim returns while another sequence holds the arm.
var errArmBusy = errors.New("a sequence is already running")

// claim takes the arm for who and returns the cancelCtx the caller's motion
// must watch. It fails with errArmBusy, naming the current holder, when someone
// else has the arm. Both happen under mu, the same lock signalCancel holds while
// it checks the holder and replaces cancelCtx, so a cancel lands either before
// the claim (and finds nothing to stop) or after it (and cancels exactly the
// context this caller got). Every successful claim is paired with one release.
func (l *armLease) claim(who string) (context.Context, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder != "" {
		return nil, fmt.Errorf("%w (%s holds the arm)", errArmBusy, l.holder)
	}
	l.holder = who
	return l.cancelCtx, nil
}

// release gives the arm back.
func (l *armLease) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holder = ""
}

// holderName names who holds the arm, or "" when it is free.
func (l *armLease) holderName() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holder
}

// busy reports whether anyone holds the arm.
func (l *armLease) busy() bool {
	return l.holderName() != ""
}
