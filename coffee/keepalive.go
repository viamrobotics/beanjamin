package coffee

// Keep-alive: a periodic short hold of the espresso machine's 1 CUP button that
// resets the machine's own idle timer, so it never falls out of STANDBY (brew
// temperature) during working hours.
//
// The machine enters POWER SAVE after an hour idle and powers off completely
// after four, and the one-hour sleep is not disableable in its settings — so
// something physical has to touch it. Holding the 1 CUP button runs water
// through the group head, which is Breville's own documented purge procedure and
// is unambiguously "use" as far as the firmware's idle timer is concerned.
//
// The arm keeps the portafilter in its claws throughout, which is why the purge
// poses live on the filter switch rather than the claws switch: the portafilter
// is the frame being positioned. Nothing is parked in the group head, so no
// filter basket gets wet and none of the rewind recovery state is touched.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.viam.com/rdk/logging"
)

// defaultKeepAliveDays is the window's weekday set when days is unset.
var defaultKeepAliveDays = []string{"mon", "tue", "wed", "thu", "fri"}

// weekdayNames maps configured day names onto time.Weekday.
var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday,
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
}

// keepAliveWindow is a KeepAlive schedule resolved into the form the tick needs:
// the location, open/close as minutes since midnight, and the weekday set. Built
// once at construction so a tick does no parsing and no repeated LoadLocation.
type keepAliveWindow struct {
	loc      *time.Location
	startMin int
	endMin   int
	days     map[time.Weekday]bool
}

// parseClock parses a "HH:MM" 24-hour time into minutes since midnight.
func parseClock(s string) (int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, fmt.Errorf("time %q must be HH:MM", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("time %q: hour must be 00-23", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("time %q: minute must be 00-59", s)
	}
	return h*60 + m, nil
}

// newKeepAliveWindow resolves a KeepAlive schedule, returning an error for any
// field that cannot be parsed. Config validation calls this and discards the
// result, so a bad schedule is rejected at config time rather than at 3am.
func newKeepAliveWindow(ka *KeepAlive) (*keepAliveWindow, error) {
	// An empty name is not a missing timezone to time.LoadLocation — it resolves
	// to UTC without error. Rejecting it here is the difference between an
	// operator who forgot the field being told so, and their window silently
	// meaning 07:45 UTC.
	if strings.TrimSpace(ka.Timezone) == "" {
		return nil, fmt.Errorf("timezone is required (an IANA name, e.g. \"America/New_York\")")
	}
	loc, err := time.LoadLocation(ka.Timezone)
	if err != nil {
		return nil, fmt.Errorf("timezone %q: %w", ka.Timezone, err)
	}
	startMin, err := parseClock(ka.AutoStart)
	if err != nil {
		return nil, fmt.Errorf("auto_start: %w", err)
	}
	endMin, err := parseClock(ka.End)
	if err != nil {
		return nil, fmt.Errorf("end: %w", err)
	}
	if startMin >= endMin {
		return nil, fmt.Errorf("auto_start %q must be earlier than end %q", ka.AutoStart, ka.End)
	}

	names := ka.Days
	if len(names) == 0 {
		names = defaultKeepAliveDays
	}
	days := make(map[time.Weekday]bool, len(names))
	for _, n := range names {
		wd, ok := weekdayNames[strings.ToLower(strings.TrimSpace(n))]
		if !ok {
			return nil, fmt.Errorf("days: %q is not one of sun/mon/tue/wed/thu/fri/sat", n)
		}
		days[wd] = true
	}

	return &keepAliveWindow{loc: loc, startMin: startMin, endMin: endMin, days: days}, nil
}

const (
	// defaultKeepAliveAfterMin is the idle time before a purge is due, in
	// minutes, when after_min is unset. The machine sleeps after roughly 60 idle
	// minutes, so this leaves room for a skipped tick.
	defaultKeepAliveAfterMin = 40.0
	// defaultKeepAliveCheckIntervalMin is the tick period when unset.
	defaultKeepAliveCheckIntervalMin = 5.0
	// defaultKeepAliveHoldSec is how long the arm holds the 1 CUP button when
	// unset. Deliberately shorter than Breville's documented 5-second purge: the
	// purge only has to make the pump run so the firmware counts it as use, not
	// stabilize group-head temperature.
	defaultKeepAliveHoldSec = 1.0
	// keepAliveMarginLimitMin bounds after_min + 2*check_interval_min. The
	// machine sleeps after ~60 idle minutes, and the margin has to absorb the
	// tick period, one tick skipped because an order was running, and the purge's
	// own duration. after_min 50 with a 10-minute interval lands exactly on the
	// cliff with nothing to spare, which is the pairing this rejects.
	keepAliveMarginLimitMin = 55.0
)

// idleThreshold is how long the machine may go unused before a purge is due.
func (ka *KeepAlive) idleThreshold() time.Duration {
	return time.Duration(orDefault(ka.AfterMin, defaultKeepAliveAfterMin) * float64(time.Minute))
}

// checkInterval is the keep-alive loop's tick period.
func (ka *KeepAlive) checkInterval() time.Duration {
	return time.Duration(orDefault(ka.CheckIntervalMin, defaultKeepAliveCheckIntervalMin) * float64(time.Minute))
}

// hold is how long the arm dwells on the 1 CUP button, which sets how much water
// each purge sends to the drip tray.
func (ka *KeepAlive) hold() time.Duration {
	return time.Duration(orDefault(ka.HoldSec, defaultKeepAliveHoldSec) * float64(time.Second))
}

// validate checks a KeepAlive block on its own: the schedule must parse, and the
// idle threshold must leave enough margin below the machine's ~60-minute sleep.
func (ka *KeepAlive) validate(path string) error {
	if _, err := newKeepAliveWindow(ka); err != nil {
		return fmt.Errorf("%s: keepalive: %w", path, err)
	}
	margin := orDefault(ka.AfterMin, defaultKeepAliveAfterMin) +
		2*orDefault(ka.CheckIntervalMin, defaultKeepAliveCheckIntervalMin)
	if margin >= keepAliveMarginLimitMin {
		return fmt.Errorf("%s: keepalive: after_min + 2*check_interval_min = %.0f must be < %.0f — "+
			"the machine sleeps after roughly 60 idle minutes and the margin has to absorb the tick "+
			"period, a tick skipped by a running order, and the purge itself",
			path, margin, keepAliveMarginLimitMin)
	}
	return nil
}

// validateKeepAlive checks the keepalive block against the rest of the config.
// Split out from Config.Validate so it is testable without building a Config
// that satisfies every unrelated requirement.
func validateKeepAlive(cfg *Config, path string) error {
	if cfg.KeepAlive == nil {
		return nil
	}
	if !cfg.HasSeparateBrewButtons {
		return fmt.Errorf("%s: keepalive requires has_separate_brew_buttons — the purge is a "+
			"timed hold of one momentary button, and holding the single-toggle machine's switch "+
			"instead pours an uncontrolled dose", path)
	}
	return cfg.KeepAlive.validate(path)
}

// contains reports whether t falls inside the window. The interval is half-open,
// [start, end) — a purge fired at exactly the closing minute would run with the
// window already shut.
//
// t is converted into the window's own location before the comparison, so the
// window means the same local wall-clock hours all year and does not shift under
// a daylight-saving transition.
func (w *keepAliveWindow) contains(t time.Time) bool {
	local := t.In(w.loc)
	if !w.days[local.Weekday()] {
		return false
	}
	minOfDay := local.Hour()*60 + local.Minute()
	return minOfDay >= w.startMin && minOfDay < w.endMin
}

// machineActivityFile is the file inside VIAM_MODULE_DATA holding the last time
// water ran through the machine.
const machineActivityFile = "machine-activity"

// machineActivityStore tracks when the machine last saw use — a brew or a purge,
// both of which reset the machine's own idle timer.
//
// Held in memory and mirrored to VIAM_MODULE_DATA, the RDK's per-machine,
// per-module directory, which survives restarts, reconfigures, and module
// version upgrades. Persisting matters because the coffee service is
// AlwaysRebuild: without it, an afternoon of config edits would fire a spurious
// purge after every reload.
//
// The zero time means "never", which makes any elapsed check due.
type machineActivityStore struct {
	mu   sync.Mutex
	last time.Time
	// path is "" when VIAM_MODULE_DATA is unset (tests, a local main.go run), in
	// which case the store is in-memory only.
	path string
}

// newMachineActivityStore builds the store, loading any previously persisted
// timestamp. Every failure to read degrades to "never used" and logs — a purge
// costs a second of arm motion, so there is nothing here worth failing
// construction over.
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

// get returns the last recorded activity time.
func (a *machineActivityStore) get() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

// record stamps t as the last activity and best-effort persists it. A write
// failure is logged and swallowed: the in-memory value is still correct, so the
// only consequence is one extra purge after the next restart.
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

// purgeSteps is the hold of the machine's 1 CUP button: free-plan into the
// standoff with the portafilter in hand, straight in and dwell so the pump runs,
// straight back out, then home.
//
// Ending at home rather than the standoff matters: the arm's resting pose is
// home, every other sequence returns there (prepareDrink's final step), and
// leaving it parked at the machine face would make the next order plan from an
// unusual configuration.
//
// Only the two linear moves carry filterCoffeeButtonCollisions. The button sits
// on the machine face behind coffee-machine-buffer-front, so the press and the
// retreat are inside that obstacle and the planner rejects them without the
// allowance.
//
// The approach must not carry it, for exactly the reason brewButtonSteps'
// approach doesn't: an allowed-collision entry is a property of the whole plan,
// not of the neighbourhood of its goal — buildConstraints hands the planner a
// CollisionSpecification covering the entire trajectory. The approach is
// free-planned from home, and allowing coffee-machine-buffer-front across that
// traverse lets the planner route the portafilter through the machine's front
// face.
// purgeHold is how long to dwell on the 1 CUP button. Falls back to the default
// when keepalive is unconfigured, so the keepalive_purge action can be used to
// verify the purge poses before the loop is ever switched on.
func (s *beanjaminCoffee) purgeHold() time.Duration {
	if s.cfg.KeepAlive == nil {
		return time.Duration(defaultKeepAliveHoldSec * float64(time.Second))
	}
	return s.cfg.KeepAlive.hold()
}

func (s *beanjaminCoffee) purgeSteps() []Step {
	hold := s.purgeHold()
	return []Step{
		{PoseName: filterPosePurgeApproach, PoseSwitch: s.filterSw},
		{PoseName: filterPosePurgePress, PoseSwitch: s.filterSw, Pause: hold,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterCoffeeButtonCollisions},
		{PoseName: filterPosePurgeApproach, PoseSwitch: s.filterSw,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterCoffeeButtonCollisions},
		// Free traverse back to the resting pose: no constraint, no allowances, so
		// it plans clear of the machine with the free-move collision buffer.
		{PoseName: filterPoseHome, PoseSwitch: s.filterSw},
	}
}

// keepAlivePurgeTimeout bounds one purge. Generous next to four short moves; if
// it is ever hit, something is wedged and the loop must not keep holding the
// `running` flag that the order queue waits on.
const keepAlivePurgeTimeout = 2 * time.Minute

// keepAlivePurgeWarningDelay is how long to wait between announcing a purge and
// actually moving, so anyone standing at the machine has a moment to step back.
const keepAlivePurgeWarningDelay = 5 * time.Second

const keepAlivePurgeAnnouncement = "Heads up — I'm about to move to keep the coffee machine warm. Please stand clear."

// keepAliveState is the snapshot one tick decides from. Pulled out so the
// decision is a pure function, testable without a service or an arm.
type keepAliveState struct {
	now          time.Time
	lastActivity time.Time
	busy         bool // an order or another sequence holds the running flag
	paused       bool // the operator cancelled; nothing moves until 'proceed'
	queued       int
}

// shouldPurge reports whether this tick should run a purge, and when it should
// not, why — the reason goes straight into the skip log, which is the only way to
// tell a quiet loop from a broken one.
//
// The checks are ordered cheapest and most-common first. Queued orders defer
// because one is about to run and reset the machine's idle timer anyway; a purge
// would only make it wait.
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

// purge holds the machine's 1 CUP button so the pump runs and the machine's idle
// timer resets. This is the motion only: the caller owns the `running` gate, the
// cancel context, and the step label.
//
// Its signature matches the execute_action map, which is how an operator drives
// one purge on demand to verify the poses (executeAction already takes the
// running gate and snapshots cancelCtx before dispatching, so a purge that took
// the gate itself would deadlock against it).
func (s *beanjaminCoffee) purge(ctx, cancelCtx context.Context) error {
	// Warn before moving. Every other arm motion in this service answers a request
	// someone just made; a purge fires on a timer, so whoever is at the machine has
	// no reason to expect it. Spoken through sayAlways rather than say: this is a
	// safety notice, not status narration an external orchestrator might want to
	// own, so conversational mode must not silence it.
	//
	// The line is queued asynchronously (say_async), so the delay starts when the
	// speech service accepts it rather than when it finishes playing.
	if err := s.sayAlways(ctx, keepAlivePurgeAnnouncement); err != nil {
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
		// The purge never parks the portafilter in the machine, so there is no
		// recovery to do beyond leaving the arm somewhere the next order can plan
		// from.
		homeStep := Step{PoseName: filterPoseHome, PoseSwitch: s.filterSw}
		if homeErr := s.executeStep(ctx, cancelCtx, homeStep); homeErr != nil {
			s.logger.Warnf("keepalive: returning to home after a failed purge: %v", homeErr)
		}
		return err
	}

	// The water went to the drip tray, so the tray-emptying counter has to see it
	// or the maintenance interval quietly under-reports.
	s.incrementSensorReading(ctx, s.usageSensor, "drip tray", "drip_tray_brews", 1)
	// Water ran, so the machine's idle timer restarted — including for a purge an
	// operator triggered by hand, which is just as much machine use as a scheduled
	// one and should push the next tick out by the full threshold.
	s.recordMachineActivity()
	return nil
}

// runPurge is the keep-alive loop's entry point: it takes the same `running`
// compare-and-swap that prepareDrink takes, so to the order queue a purge is
// indistinguishable from an order and one arriving mid-purge waits rather than
// planning against an arm that is already moving.
//
// Releasing that flag on every path is load-bearing — a purge that returned while
// holding it would stall the queue permanently, which is also why the motion runs
// under a timeout.
func (s *beanjaminCoffee) runPurge(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("keepalive: a sequence is already running")
	}
	defer s.running.Store(false)

	// Snapshot the cancel context under the mutex, as every other sequence does,
	// so an operator cancel interrupts the moves mid-trajectory.
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
// machine. Called after a successful brew and after a successful purge, both of
// which reset the machine's own idle timer. No-op when keepalive is unconfigured.
func (s *beanjaminCoffee) recordMachineActivity() {
	if s.machineActivity == nil {
		return
	}
	s.machineActivity.record(s.logger, time.Now())
}

// keepAliveLoop is the background ticker that keeps the machine at brew
// temperature. Started by NewCoffee only when keepalive is configured, and it
// runs for the life of the service.
//
// It deliberately does not watch cancelCtx. An operator cancel pauses the queue
// rather than shutting the service down, and shouldPurge already declines while
// paused — so the loop keeps ticking and resumes on its own once 'proceed'
// arrives. queueStop, closed by Close, is the shutdown signal, and Close also
// cancels cancelCtx, which aborts any purge mid-move.
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
			// Back off to the next tick rather than retrying immediately: whatever
			// blocked the arm is unlikely to clear in the same second.
			continue
		}
	}
}

// notifyKeepAliveFailureSlack sends a plain-text Slack message for a failed
// purge. Text-only rather than the Block Kit layout notifyOrderFailureSlack
// builds: there is no order, customer, or clip to link, just one
// operator-actionable fact.
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
		if _, err := s.slackNotifier.DoCommand(ctx, map[string]any{
			"command": "send",
			"text":    text,
		}); err != nil {
			logger.Warnf("keepalive: slack notify failed: %v", err)
		}
	}()
}
