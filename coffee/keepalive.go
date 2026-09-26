package coffee

// Keep-alive: a periodic short hold of the machine's 1 CUP button. It runs water
// through the group head — Breville's documented purge — which resets the
// machine's own idle timer and keeps it at brew temperature. The machine sleeps
// after an hour idle and that hour is not configurable, so something physical has
// to touch it.
//
// The arm holds the portafilter throughout, so the purge poses are filter-frame
// poses on the filter switch and nothing is parked in the group head.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.viam.com/rdk/logging"
)

const machineActivityFile = "machine-activity"

// machineActivityStore tracks when water last ran through the machine — a brew or
// a purge. Mirrored to VIAM_MODULE_DATA because the service is AlwaysRebuild:
// without persisting, every reconfigure would fire a spurious purge. The zero
// time means "never", which makes any elapsed check due.
type machineActivityStore struct {
	mu   sync.Mutex
	last time.Time
	// path is "" when VIAM_MODULE_DATA is unset (tests, a local run): in-memory only.
	path string
}

// newMachineActivityStore loads any persisted timestamp. Every read failure
// degrades to "never used" — a purge is cheap, a wedged coffee service is not.
func newMachineActivityStore(logger logging.Logger) *machineActivityStore {
	a := &machineActivityStore{}

	dir := os.Getenv("VIAM_MODULE_DATA")
	if dir == "" {
		logger.Warnf("keepalive: VIAM_MODULE_DATA is unset — machine activity will be tracked in " +
			"memory only and a reconfigure will trigger one extra purge")
		return a
	}
	a.path = filepath.Join(dir, machineActivityFile)

	raw, err := os.ReadFile(a.path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warnf("keepalive: reading %s: %v — treating the machine as long idle", a.path, err)
		}
		return a
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
	if err != nil {
		logger.Warnf("keepalive: %s holds %q, which is not an RFC3339 time: %v — treating the "+
			"machine as long idle", a.path, string(raw), err)
		return a
	}

	a.last = t
	logger.Infof("keepalive: last machine activity was %s (loaded from %s)", t.Format(time.RFC3339), a.path)
	return a
}

func (a *machineActivityStore) get() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

// record persists best-effort: a write failure leaves the in-memory value correct,
// costing one extra purge after the next restart.
func (a *machineActivityStore) record(logger logging.Logger, t time.Time) {
	a.mu.Lock()
	a.last = t
	path := a.path
	a.mu.Unlock()

	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(t.Format(time.RFC3339)), 0o644); err != nil {
		logger.Warnf("keepalive: persisting machine activity to %s: %v", path, err)
	}
}

// purgeHold falls back to the default when keepalive is unconfigured, so the
// keepalive_purge action can verify the poses before the loop is switched on.
func (s *beanjaminCoffee) purgeHold() time.Duration {
	if s.cfg.KeepAlive == nil {
		return time.Duration(defaultKeepAliveHoldSec * float64(time.Second))
	}
	return s.cfg.KeepAlive.hold()
}

// purgeSteps: standoff, straight in and dwell so the pump runs, straight back
// out, then home (the arm's resting pose — leaving it at the machine face would
// make the next order plan from an unusual configuration).
//
// Only the two linear moves may allow filterCoffeeButtonCollisions. An
// allowed-collision entry covers the whole trajectory, not the neighbourhood of
// its goal, so carrying it on the free-planned approach or the trip home would
// let the planner route the portafilter through the machine.
func (s *beanjaminCoffee) purgeSteps() []Step {
	hold := s.purgeHold()
	return []Step{
		{PoseName: filterPosePurgeApproach, PoseSwitch: s.filterSw},
		{PoseName: filterPosePurgePress, PoseSwitch: s.filterSw, Pause: hold,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterCoffeeButtonCollisions},
		{PoseName: filterPosePurgeApproach, PoseSwitch: s.filterSw,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterCoffeeButtonCollisions},
		{PoseName: filterPoseHome, PoseSwitch: s.filterSw},
	}
}

// keepAlivePurgeTimeout bounds one purge: if it is hit, something is wedged and
// the loop must not keep holding the `running` flag the order queue waits on.
const keepAlivePurgeTimeout = 2 * time.Minute

// keepAlivePurgeWarningDelay gives anyone at the machine a moment to step back.
const keepAlivePurgeWarningDelay = 5 * time.Second

const keepAlivePurgeAnnouncement = "Heads up — I'm about to move to keep the coffee machine warm. Please stand clear."

// keepAliveState is one tick's snapshot, so the decision is a pure function.
type keepAliveState struct {
	now          time.Time
	lastActivity time.Time
	busy         bool // an order or another sequence holds the running flag
	paused       bool // a cancel or an order fault; nothing moves until 'proceed'
	queued       int
}

// shouldPurge reports whether to purge, and if not, why — the reason is logged,
// which is the only way to tell a quiet loop from a broken one. Queued orders
// defer because one is about to reset the machine's timer anyway.
func shouldPurge(w *keepAliveWindow, threshold time.Duration, st keepAliveState) (bool, string) {
	if !w.contains(st.now) {
		return false, "outside the keep-alive window"
	}
	if st.busy {
		return false, "a sequence is already running"
	}
	if st.paused {
		return false, "the queue is paused"
	}
	if st.queued > 0 {
		return false, fmt.Sprintf("%d order(s) queued — one will reset the machine's timer", st.queued)
	}
	if idle := st.now.Sub(st.lastActivity); idle < threshold {
		return false, fmt.Sprintf("machine used %s ago, threshold is %s", idle.Round(time.Second), threshold)
	}
	return true, ""
}

// purge is the motion only — the caller owns the `running` gate, the cancel
// context, and the step label. The signature matches the execute_action map,
// which already holds that gate before dispatching, so a purge taking the gate
// itself would deadlock against it.
func (s *beanjaminCoffee) purge(ctx, cancelCtx context.Context) error {
	// A purge fires on a timer, so whoever is at the machine has no reason to
	// expect it. SayAlways, not Say: a safety notice, not status narration, so
	// conversational mode must not silence it. say_async returns on queueing, so
	// the delay starts when the line is accepted, not when it finishes playing.
	if err := s.speaker.SayAlways(ctx, keepAlivePurgeAnnouncement); err != nil {
		s.logger.Warnf("keepalive: purge announcement failed: %v", err)
	}
	select {
	case <-time.After(keepAlivePurgeWarningDelay):
	case <-ctx.Done():
		return fmt.Errorf("keepalive: cancelled during the pre-purge warning: %w", ctx.Err())
	case <-cancelCtx.Done():
		return errors.New("keepalive: cancelled during the pre-purge warning")
	}

	if err := s.runSteps(ctx, cancelCtx, "keepalive_purge", s.purgeSteps()...); err != nil {
		// Nothing is parked in the machine, so recovery is just getting back to a
		// pose the next order can plan from.
		homeStep := Step{PoseName: filterPoseHome, PoseSwitch: s.filterSw}
		if homeErr := s.executeStep(ctx, cancelCtx, homeStep); homeErr != nil {
			s.logger.Warnf("keepalive: returning to home after a failed purge: %v", homeErr)
		}
		return err
	}

	// Water reached the drip tray, so the tray counter has to see it.
	s.incrementSensorReading(ctx, s.usageSensor, "drip tray", "drip_tray_brews", 1)
	// Counts for a hand-triggered purge too: it is just as much machine use.
	s.recordMachineActivity()
	return nil
}

// runPurge is the loop's entry point. It takes the same `running` gate
// prepareDrink takes, so the queue treats a purge like an order; releasing that
// gate on every path is load-bearing, since holding it would stall the queue
// permanently.
func (s *beanjaminCoffee) runPurge(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("keepalive: a sequence is already running")
	}
	defer s.running.Store(false)

	// Snapshot cancelCtx under the mutex, as every other sequence does, so an
	// operator cancel interrupts the moves mid-trajectory.
	s.mu.Lock()
	cancelCtx := s.cancelCtx
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, keepAlivePurgeTimeout)
	defer cancel()

	s.setStep(stepKeepAlive)
	defer s.setStep("")

	return s.purge(ctx, cancelCtx)
}

// recordMachineActivity stamps now as the last time water ran through the
// machine. No-op when keepalive is unconfigured.
func (s *beanjaminCoffee) recordMachineActivity() {
	if s.machineActivity == nil {
		return
	}
	s.machineActivity.record(s.logger, time.Now())
}

// keepAliveLoop runs for the life of the service, started by NewCoffee only when
// keepalive is configured.
//
// It watches queueStop rather than cancelCtx: a cancel pauses the queue rather
// than shutting down, shouldPurge already declines while paused, and cancelCtx is
// rotated under s.mu so reading it here would race.
func (s *beanjaminCoffee) keepAliveLoop(w *keepAliveWindow) {
	ka := s.cfg.KeepAlive
	interval := ka.checkInterval()
	threshold := ka.idleThreshold()
	s.logger.Infof("keepalive: purging the group head when the machine has been idle %s; checking "+
		"every %s inside %s-%s %s", threshold, interval, ka.AutoStart, ka.End, ka.Timezone)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.queueStop:
			s.logger.Infof("keepalive: shutting down")
			return
		case <-ticker.C:
		}

		st := keepAliveState{
			now:          time.Now(),
			lastActivity: s.machineActivity.get(),
			busy:         s.running.Load(),
			paused:       s.paused.Load(),
			queued:       s.queue.Len(),
		}
		ok, why := shouldPurge(w, threshold, st)
		if !ok {
			s.logger.Debugf("keepalive: skipping this tick — %s", why)
			continue
		}

		idle := st.now.Sub(st.lastActivity).Round(time.Second)
		s.logger.Infof("keepalive: machine idle %s — purging the group head to hold brew temperature", idle)
		if err := s.runPurge(context.Background()); err != nil {
			s.logger.Errorf("keepalive: purge failed, the machine may drop out of brew temperature: %v", err)
			s.notifyKeepAliveFailureSlack(err)
			// Back off to the next tick; whatever blocked the arm won't clear instantly.
			continue
		}
	}
}

// notifyKeepAliveFailureSlack sends a plain-text message: unlike a failed order
// there is no customer or clip to link, just one operator-actionable fact.
func (s *beanjaminCoffee) notifyKeepAliveFailureSlack(purgeErr error) {
	if s.slackNotifier == nil {
		return
	}
	text := fmt.Sprintf(":warning: Keep-alive purge failed — the espresso machine may drop out of "+
		"brew temperature and the next order could brew cold. Error: %v", purgeErr)
	logger := s.logger
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notifySlackTimeout)
		defer cancel()
		if _, err := s.slackNotifier.Send(ctx, text, nil); err != nil {
			logger.Warnf("keepalive: slack notify failed: %v", err)
		}
	}()
}
