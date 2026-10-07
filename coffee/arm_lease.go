package coffee

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// armLease groups the state that decides who may move the arm, how to
// interrupt them, and whether the queue may start the next order:
//
//   - holder names who has the arm, "" when it is free. Every sequence that
//     moves the arm or swaps the frame system takes it with a claim, which
//     also hands back cancelCtx, and gives it back with release.
//   - cancelCtx is the shared context motion steps watch. signalCancel
//     cancels it through cancelFunc and replaces both.
//   - paused holds automated work back after a cancel or a fault until
//     proceed.
//
// All of it is guarded by mu, so every caller gets one consistent answer to
// "can I run now?". There are two kinds of caller:
//
//   - Automated work (the order queue and the keepalive purge) claims with
//     claimAutomated, which is refused while paused.
//   - Troubleshooting (execute_action, run_cup_flow, rewind, reset_world,
//     proceed and the gripper actions) claims with claimManual, which ignores
//     the pause so an operator can recover the machine.
type armLease struct {
	mu         sync.Mutex
	cancelCtx  context.Context
	cancelFunc func()
	holder     string // who holds the arm, for logs, errors and Status; "" when free
	paused     bool
	// closed is set by shutdown and refuses every later claim, so nothing can
	// take the arm after the service starts closing.
	closed bool
}

var (
	// errArmBusy is returned by a claim while another sequence holds the arm.
	errArmBusy = errors.New("a sequence is already running")
	// errQueuePaused is returned by an automated claim while paused.
	errQueuePaused = errors.New("the queue is paused — send 'proceed' to resume")
	// errServiceClosed is returned by any claim once the service is closing.
	errServiceClosed = errors.New("service closing")
)

// claimManual takes the arm for a troubleshooting command and returns the
// cancelCtx the caller's motion must watch. It ignores the pause, and fails with
// errArmBusy, naming the current holder, when someone else has the arm. Both
// happen under mu, the same lock signalCancel holds while it checks the holder
// and replaces cancelCtx, so a cancel lands either before the claim (and finds
// nothing to stop) or after it (and cancels exactly the context this caller
// got). Every successful claim is paired with one release.
func (l *armLease) claimManual(who string) (context.Context, error) {
	return l.claim(who, false)
}

// claimAutomated takes the arm for the order queue or the keepalive purge. On
// top of claimManual's rule it fails with errQueuePaused while paused, in the
// same locked step, so nothing automated can start between a cancel and
// proceed.
func (l *armLease) claimAutomated(who string) (context.Context, error) {
	return l.claim(who, true)
}

func (l *armLease) claim(who string, respectPause bool) (context.Context, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, errServiceClosed
	}
	if l.holder != "" {
		return nil, fmt.Errorf("%w (%s holds the arm)", errArmBusy, l.holder)
	}
	if respectPause && l.paused {
		return nil, errQueuePaused
	}
	l.holder = who
	return l.cancelCtx, nil
}

// pause holds automated work back without interrupting anyone. A faulted order
// calls it while the queue still holds the arm.
func (l *armLease) pause() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paused = true
}

// unpause lifts the pause, reporting whether there was one to lift, so two
// callers racing cannot both claim to have released a single pause.
func (l *armLease) unpause() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	was := l.paused
	l.paused = false
	return was
}

// shutdown refuses every later claim and cancels whoever holds the arm. Close
// calls it.
func (l *armLease) shutdown() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.cancelFunc != nil {
		l.cancelFunc()
	}
}

// isPaused reports whether automated work is held back until proceed.
func (l *armLease) isPaused() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.paused
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
