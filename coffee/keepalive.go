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
	"strconv"
	"strings"
	"time"
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
