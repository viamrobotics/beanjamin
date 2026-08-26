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
