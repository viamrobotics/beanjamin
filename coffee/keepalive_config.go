package coffee

// Keep-alive configuration: the keepalive attribute, its validation, and the
// weekly time window it resolves to. The purge loop itself is keepalive.go.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// KeepAlive configures the idle-purge loop that holds the espresso machine at
// brew temperature (keepalive.go). Presence enables the loop; nil disables it.
//
// AutoStart must mirror the time programmed into the machine's own Auto Start
// setting, and is also the window's open. Deliberately one number: as two
// settings they drift, and a window opening after Auto Start leaves the machine
// awake long enough to fall into POWER SAVE before anyone can order.
type KeepAlive struct {
	// AutoStart / End bound the window as "HH:MM" local times, half-open.
	AutoStart string `json:"auto_start"`
	End       string `json:"end"`
	// Timezone is a required IANA name, so the window does not depend on host TZ.
	Timezone string `json:"timezone"`
	// Days are three-letter weekday names; defaults to Monday–Friday.
	Days []string `json:"days,omitempty"`

	AfterMin         float64 `json:"after_min,omitempty"`
	CheckIntervalMin float64 `json:"check_interval_min,omitempty"`
	// HoldSec sets the water volume per purge — the knob if the tray fills fast.
	HoldSec float64 `json:"hold_sec,omitempty"`
}

var defaultKeepAliveDays = []string{"mon", "tue", "wed", "thu", "fri"}

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday,
	"mon": time.Monday,
	"tue": time.Tuesday,
	"wed": time.Wednesday,
	"thu": time.Thursday,
	"fri": time.Friday,
	"sat": time.Saturday,
}

// keepAliveWindow is a KeepAlive schedule resolved for the tick: location,
// open/close as minutes since midnight, weekday set. Built once so a tick does no
// parsing and no repeated LoadLocation.
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

// newKeepAliveWindow resolves a schedule, erroring on any unparseable field.
// Config validation calls it and discards the result, so a bad schedule is
// rejected at config time rather than at 3am.
func newKeepAliveWindow(ka *KeepAlive) (*keepAliveWindow, error) {
	// time.LoadLocation("") is UTC, not an error, so an omitted timezone has to be
	// rejected here or the window silently means UTC hours.
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
	defaultKeepAliveAfterMin         = 40.0
	defaultKeepAliveCheckIntervalMin = 5.0
	// defaultKeepAliveHoldSec is shorter than Breville's documented 5-second
	// purge: this only has to make the pump run, not stabilize temperature.
	defaultKeepAliveHoldSec = 1.0
	// keepAliveMarginLimitMin bounds after_min + 2*check_interval_min. The machine
	// sleeps after ~60 idle minutes and the margin must absorb the tick period, a
	// tick skipped by a running order, and the purge itself.
	keepAliveMarginLimitMin = 55.0
)

func (ka *KeepAlive) idleThreshold() time.Duration {
	return time.Duration(orDefault(ka.AfterMin, defaultKeepAliveAfterMin) * float64(time.Minute))
}

func (ka *KeepAlive) checkInterval() time.Duration {
	return time.Duration(orDefault(ka.CheckIntervalMin, defaultKeepAliveCheckIntervalMin) * float64(time.Minute))
}

// hold is the dwell on the 1 CUP button, which sets how much water each purge
// sends to the drip tray.
func (ka *KeepAlive) hold() time.Duration {
	return time.Duration(orDefault(ka.HoldSec, defaultKeepAliveHoldSec) * float64(time.Second))
}

// validate checks the schedule parses and the idle threshold leaves enough margin
// below the machine's ~60-minute sleep.
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

// validateKeepAlive checks the block against the rest of the config. Split from
// Config.Validate so it is testable without a fully-valid Config.
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

// contains reports whether t is in the window. Half-open, [start, end). t is
// converted into the window's location first, so the hours mean the same local
// wall-clock time across a daylight-saving transition.
func (w *keepAliveWindow) contains(t time.Time) bool {
	local := t.In(w.loc)
	if !w.days[local.Weekday()] {
		return false
	}
	minOfDay := local.Hour()*60 + local.Minute()
	return minOfDay >= w.startMin && minOfDay < w.endMin
}
