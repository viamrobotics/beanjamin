# Espresso Machine Keep-Alive Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the espresso machine dropping out of brew temperature during working hours, by having the arm periodically hold the machine's 1 CUP button to reset its own 1-hour idle timer.

**Architecture:** A background goroutine in the coffee service ticks every few minutes. When it is inside a configured weekday window, nothing else is running, and no water has gone through the machine for ~40 minutes, it holds the machine's 1 CUP button for ~1 second — a documented Breville group-head purge. The arm keeps the portafilter in its claws throughout, so the press is a *filter*-frame pose and no basket gets wet. Last-activity time persists in `VIAM_MODULE_DATA` so it survives reconfigures.

**Tech Stack:** Go, Viam RDK (`go.viam.com/rdk`), standard library only. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-26-machine-keepalive-design.md`

## Global Constraints

- Package is `coffee`; every file in this plan lives in `coffee/` except `README.md`.
- Tests are standard-library `testing` only — no testify, no gomock. Table-driven with `t.Run` subtests, matching `coffee/brew_button_test.go` and `coffee/config_test.go`.
- Numeric config tunables use the house pattern: a `float64` field with `omitempty`, plus a small getter on the type that returns the configured value or a default constant, resolved through the existing generic `orDefault[T ~int | ~float64](v, def T) T` in `coffee/config.go`.
- Comments explain *why*, never the change history. No "previously", "now we", "added".
- `make lint` runs `gofmt -s -w .` in place and must be run before each commit. `make test` is `go test ./...`.
- The feature is entirely opt-in: `Config.KeepAlive == nil` must leave every existing code path byte-identical in behavior, including `requiredPoses()`. The other machine in the fleet must not fail construction after this change.
- `hold_sec` default is `1.0`. This is deliberately shorter than Breville's documented 5-second purge: the purge only needs the pump to run so the firmware counts it as use, not to stabilize group-head temperature.
- Never press the POWER button anywhere in this feature. Per the manual, pressing POWER while the machine is in POWER SAVE turns it *off*.

---

### Task 1: Window parsing and membership

The schedule is parsed once into a resolved struct so the tick does no parsing and no repeated `time.LoadLocation`. Membership converts the instant into the window's own location and compares wall-clock minutes, which is what keeps it correct across DST.

**Files:**
- Create: `coffee/keepalive.go`
- Create: `coffee/keepalive_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type keepAliveWindow struct { loc *time.Location; startMin, endMin int; days map[time.Weekday]bool }`
  - `func parseClock(s string) (int, error)` — "HH:MM" to minutes since midnight
  - `func newKeepAliveWindow(ka *KeepAlive) (*keepAliveWindow, error)`
  - `func (w *keepAliveWindow) contains(t time.Time) bool`
  - `var defaultKeepAliveDays = []string{"mon", "tue", "wed", "thu", "fri"}`
  - `type KeepAlive struct` and the `Config.KeepAlive` field, both added to `coffee/config.go` in Step 1 below. Every later task consumes these; only the getters and validation are added later, in Task 2.

- [ ] **Step 1: Add the `KeepAlive` config struct**

In `coffee/config.go`, immediately after the `ContainerDimensions` type and its `validate` method (near the end of the type declarations, before `func (cfg *Config) Validate`), add:

```go
// KeepAlive configures the idle-purge loop that keeps the espresso machine at
// brew temperature (keepalive.go). Presence enables the loop; nil disables the
// whole feature.
//
// AutoStart must mirror the time programmed into the machine's own Auto Start
// setting. It is both "when the machine turns itself on" and the window's open,
// deliberately one number: as two independent settings they drift, and a window
// that opens after Auto Start leaves the machine awake and idling — long enough
// to fall into POWER SAVE before anyone can order.
type KeepAlive struct {
	// AutoStart / End bound the window, as "HH:MM" 24-hour local times. The
	// interval is half-open, [AutoStart, End).
	AutoStart string `json:"auto_start"`
	End       string `json:"end"`
	// Timezone is an IANA location name (e.g. "America/New_York"). Required, so
	// the window means the same thing regardless of the host's TZ.
	Timezone string `json:"timezone"`
	// Days are lowercase three-letter weekday names; defaults to Monday–Friday.
	Days []string `json:"days,omitempty"`

	// AfterMin is how many idle minutes must pass before a purge is due.
	AfterMin float64 `json:"after_min,omitempty"`
	// CheckIntervalMin is the tick period.
	CheckIntervalMin float64 `json:"check_interval_min,omitempty"`
	// HoldSec is how long the arm holds the 1 CUP button. It sets the water
	// volume, so it is the knob to turn if the drip tray fills too fast.
	HoldSec float64 `json:"hold_sec,omitempty"`
}
```

Then add the field to `Config`, immediately after the `DoorApproachRelativePose` field (the last field in the struct):

```go
	// KeepAlive, when set, runs the idle-purge loop (keepalive.go) that holds the
	// machine's 1 CUP button periodically so it never falls out of brew
	// temperature. Requires HasSeparateBrewButtons. Unset disables it.
	KeepAlive *KeepAlive `json:"keepalive,omitempty"`
```

- [ ] **Step 2: Write the failing tests**

Create `coffee/keepalive_test.go`:

```go
package coffee

import (
	"testing"
	"time"
)

func TestParseClock(t *testing.T) {
	valid := map[string]int{
		"00:00": 0,
		"07:45": 7*60 + 45,
		"17:00": 17 * 60,
		"23:59": 23*60 + 59,
	}
	for in, want := range valid {
		got, err := parseClock(in)
		if err != nil {
			t.Errorf("parseClock(%q) returned error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseClock(%q) = %d, want %d", in, got, want)
		}
	}

	invalid := []string{"", "7", "07:45:00", "24:00", "07:60", "-1:00", "ab:cd"}
	for _, in := range invalid {
		if _, err := parseClock(in); err == nil {
			t.Errorf("parseClock(%q) returned no error, want one", in)
		}
	}
}

// validKeepAlive is a KeepAlive that parses cleanly, for tests that mutate one
// field at a time.
func validKeepAlive() *KeepAlive {
	return &KeepAlive{
		AutoStart: "07:45",
		End:       "17:00",
		Timezone:  "America/New_York",
	}
}

func TestNewKeepAliveWindowErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*KeepAlive)
	}{
		{"unknown timezone", func(k *KeepAlive) { k.Timezone = "Mars/Olympus_Mons" }},
		{"empty timezone", func(k *KeepAlive) { k.Timezone = "" }},
		{"unparseable auto_start", func(k *KeepAlive) { k.AutoStart = "quarter to eight" }},
		{"unparseable end", func(k *KeepAlive) { k.End = "17" }},
		{"start equals end", func(k *KeepAlive) { k.AutoStart = "17:00" }},
		{"start after end", func(k *KeepAlive) { k.AutoStart = "18:00" }},
		{"unknown day", func(k *KeepAlive) { k.Days = []string{"mon", "funday"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ka := validKeepAlive()
			tt.mutate(ka)
			if _, err := newKeepAliveWindow(ka); err == nil {
				t.Errorf("newKeepAliveWindow accepted %s, want an error", tt.name)
			}
		})
	}

	// The happy path, including case-insensitive and padded day names.
	ka := validKeepAlive()
	ka.Days = []string{"Mon", " tue "}
	w, err := newKeepAliveWindow(ka)
	if err != nil {
		t.Fatalf("newKeepAliveWindow on a valid config: %v", err)
	}
	if !w.days[time.Monday] || !w.days[time.Tuesday] {
		t.Errorf("days = %v, want Monday and Tuesday set", w.days)
	}
	if w.days[time.Wednesday] {
		t.Errorf("Wednesday is set but was not configured")
	}
}

func TestKeepAliveWindowDefaultDays(t *testing.T) {
	w, err := newKeepAliveWindow(validKeepAlive())
	if err != nil {
		t.Fatalf("newKeepAliveWindow: %v", err)
	}
	for _, wd := range []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday} {
		if !w.days[wd] {
			t.Errorf("default days missing %v", wd)
		}
	}
	for _, wd := range []time.Weekday{time.Saturday, time.Sunday} {
		if w.days[wd] {
			t.Errorf("default days include %v, want weekdays only", wd)
		}
	}
}

func TestKeepAliveWindowContains(t *testing.T) {
	w, err := newKeepAliveWindow(validKeepAlive()) // 07:45–17:00 America/New_York, Mon–Fri
	if err != nil {
		t.Fatalf("newKeepAliveWindow: %v", err)
	}
	ny := w.loc

	tests := []struct {
		name string
		t    time.Time
		want bool
	}{
		// 2026-01-15 is a Thursday, 2026-01-17 a Saturday.
		{"just before open", time.Date(2026, 1, 15, 7, 44, 59, 0, ny), false},
		{"exactly at open", time.Date(2026, 1, 15, 7, 45, 0, 0, ny), true},
		{"midday", time.Date(2026, 1, 15, 12, 0, 0, 0, ny), true},
		{"minute before close", time.Date(2026, 1, 15, 16, 59, 0, 0, ny), true},
		{"exactly at close is out", time.Date(2026, 1, 15, 17, 0, 0, 0, ny), false},
		{"after close", time.Date(2026, 1, 15, 18, 0, 0, 0, ny), false},
		{"middle of the night", time.Date(2026, 1, 15, 3, 0, 0, 0, ny), false},
		{"saturday midday", time.Date(2026, 1, 17, 12, 0, 0, 0, ny), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := w.contains(tt.t); got != tt.want {
				t.Errorf("contains(%s) = %v, want %v", tt.t.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

// TestKeepAliveWindowContainsAcrossDST proves the window is compared in local
// wall-clock time, not as a fixed UTC offset. 12:30 UTC is 07:30 EST in January
// (outside a 07:45 open) but 08:30 EDT in July (inside it) — the same instant of
// day falling on opposite sides of the boundary.
func TestKeepAliveWindowContainsAcrossDST(t *testing.T) {
	w, err := newKeepAliveWindow(validKeepAlive())
	if err != nil {
		t.Fatalf("newKeepAliveWindow: %v", err)
	}

	tests := []struct {
		name string
		utc  time.Time
		want bool
	}{
		// 2026-01-15 is a Thursday (EST, UTC-5).
		{"january 12:30 UTC is 07:30 EST", time.Date(2026, 1, 15, 12, 30, 0, 0, time.UTC), false},
		{"january 13:00 UTC is 08:00 EST", time.Date(2026, 1, 15, 13, 0, 0, 0, time.UTC), true},
		// 2026-07-15 is a Wednesday (EDT, UTC-4).
		{"july 12:30 UTC is 08:30 EDT", time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC), true},
		{"july 11:30 UTC is 07:30 EDT", time.Date(2026, 7, 15, 11, 30, 0, 0, time.UTC), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := w.contains(tt.utc); got != tt.want {
				t.Errorf("contains(%s) = %v, want %v", tt.utc.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./coffee -run 'TestParseClock|TestNewKeepAliveWindow|TestKeepAliveWindow' -v`
Expected: FAIL — compile error, `undefined: parseClock`, `undefined: newKeepAliveWindow`.

- [ ] **Step 4: Write the implementation**

Create `coffee/keepalive.go`:

```go
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
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./coffee -run 'TestParseClock|TestNewKeepAliveWindow|TestKeepAliveWindow' -v`
Expected: PASS, all subtests.

- [ ] **Step 6: Lint and commit**

```bash
make lint
go build ./...
git add coffee/config.go coffee/keepalive.go coffee/keepalive_test.go
git commit -m "Add keep-alive schedule config and window membership

The window is resolved once into a location plus minutes-since-midnight
so the tick does no parsing, and membership converts the instant into the
window's location before comparing wall-clock minutes — which is what
keeps a configured 07:45 open meaning 07:45 local across DST."
```

---

### Task 2: Config validation and pose gating

Two rules live above the schedule parsing: the feature needs the button machine, and the idle threshold has to leave enough margin to survive a skipped tick. Both are extracted into a free function so they are testable without constructing a fully-valid `Config` (which would need vision services, cameras, and container dimensions).

**Files:**
- Modify: `coffee/keepalive.go`
- Modify: `coffee/config.go` — inside `func (cfg *Config) Validate`
- Modify: `coffee/espresso.go` — inside `func (s *beanjaminCoffee) requiredPoses`
- Modify: `coffee/keepalive_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `KeepAlive`, `newKeepAliveWindow` (Task 1).
- Produces:
  - `func (ka *KeepAlive) validate(path string) error`
  - `func validateKeepAlive(cfg *Config, path string) error`
  - `func (ka *KeepAlive) idleThreshold() time.Duration`
  - `func (ka *KeepAlive) checkInterval() time.Duration`
  - `func (ka *KeepAlive) hold() time.Duration`
  - Constants `defaultKeepAliveAfterMin`, `defaultKeepAliveCheckIntervalMin`, `defaultKeepAliveHoldSec`, `keepAliveMarginLimitMin`
  - Pose constants `filterPosePurgeApproach`, `filterPosePurgePress` (declared here, used by Task 4)

- [ ] **Step 1: Write the failing tests**

Append to `coffee/keepalive_test.go`:

```go
func TestKeepAliveGetters(t *testing.T) {
	// Unset fields fall back to the defaults.
	ka := validKeepAlive()
	if got, want := ka.idleThreshold(), time.Duration(defaultKeepAliveAfterMin*float64(time.Minute)); got != want {
		t.Errorf("idleThreshold() = %v, want %v", got, want)
	}
	if got, want := ka.checkInterval(), time.Duration(defaultKeepAliveCheckIntervalMin*float64(time.Minute)); got != want {
		t.Errorf("checkInterval() = %v, want %v", got, want)
	}
	if got, want := ka.hold(), time.Duration(defaultKeepAliveHoldSec*float64(time.Second)); got != want {
		t.Errorf("hold() = %v, want %v", got, want)
	}

	// Configured values win.
	ka.AfterMin = 30
	ka.CheckIntervalMin = 2
	ka.HoldSec = 1.5
	if got, want := ka.idleThreshold(), 30*time.Minute; got != want {
		t.Errorf("idleThreshold() = %v, want %v", got, want)
	}
	if got, want := ka.checkInterval(), 2*time.Minute; got != want {
		t.Errorf("checkInterval() = %v, want %v", got, want)
	}
	if got, want := ka.hold(), 1500*time.Millisecond; got != want {
		t.Errorf("hold() = %v, want %v", got, want)
	}
}

func TestValidateKeepAlive(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr bool
	}{
		{
			name:    "nil keepalive is allowed",
			cfg:     &Config{},
			wantErr: false,
		},
		{
			name:    "valid on the button machine",
			cfg:     &Config{HasSeparateBrewButtons: true, KeepAlive: validKeepAlive()},
			wantErr: false,
		},
		{
			name:    "rejected on the single-toggle machine",
			cfg:     &Config{HasSeparateBrewButtons: false, KeepAlive: validKeepAlive()},
			wantErr: true,
		},
		{
			name: "bad schedule is rejected",
			cfg: &Config{HasSeparateBrewButtons: true, KeepAlive: &KeepAlive{
				AutoStart: "07:45", End: "17:00", Timezone: "Nowhere/Nothing",
			}},
			wantErr: true,
		},
		{
			name: "margin exactly at the limit is rejected",
			cfg: &Config{HasSeparateBrewButtons: true, KeepAlive: &KeepAlive{
				AutoStart: "07:45", End: "17:00", Timezone: "UTC",
				// 45 + 2*5 = 55, which is the limit and therefore too tight.
				AfterMin: 45, CheckIntervalMin: 5,
			}},
			wantErr: true,
		},
		{
			name: "the documented 50/10 pairing is rejected",
			cfg: &Config{HasSeparateBrewButtons: true, KeepAlive: &KeepAlive{
				AutoStart: "07:45", End: "17:00", Timezone: "UTC",
				AfterMin: 50, CheckIntervalMin: 10,
			}},
			wantErr: true,
		},
		{
			name: "the default 40/5 pairing is accepted",
			cfg: &Config{HasSeparateBrewButtons: true, KeepAlive: &KeepAlive{
				AutoStart: "07:45", End: "17:00", Timezone: "UTC",
				AfterMin: 40, CheckIntervalMin: 5,
			}},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateKeepAlive(tt.cfg, "services.coffee")
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Errorf("validateKeepAlive() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./coffee -run 'TestKeepAliveGetters|TestValidateKeepAlive' -v`
Expected: FAIL — `undefined: validateKeepAlive`, `undefined: defaultKeepAliveAfterMin`.

- [ ] **Step 3: Add the defaults, getters, and validation**

Append to `coffee/keepalive.go` (the `time` import is already present from Task 1):

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./coffee -run 'TestKeepAliveGetters|TestValidateKeepAlive' -v`
Expected: PASS.

- [ ] **Step 5: Wire validation into `Config.Validate`**

In `coffee/config.go`, inside `func (cfg *Config) Validate`, immediately before the final `return reqDeps, optDeps, nil`:

```go
	if err := validateKeepAlive(cfg, path); err != nil {
		return nil, nil, err
	}
```

- [ ] **Step 6: Add the purge pose constants**

In `coffee/espresso.go`, in the `//filter pose switches` const block, after `filterPoseCoffeeShake`:

```go
	// Keep-alive purge poses (required only when keepalive is configured). Both
	// are filter-frame poses: the arm holds the portafilter while it presses the
	// machine's 1 CUP button, so the portafilter is what gets positioned.
	filterPosePurgeApproach = "purge_approach"
	filterPosePurgePress    = "purge_press"
```

- [ ] **Step 7: Gate the poses in `requiredPoses`**

In `coffee/espresso.go`, inside `func (s *beanjaminCoffee) requiredPoses`, after the `if s.cfg.CanServeDecaf { ... }` block:

```go
	// The keep-alive purge holds the 1 CUP button with the portafilter still in
	// the claws. Gated: a machine without keepalive configured never travels to
	// these poses, and requiring them unconditionally would fail construction on
	// every existing machine.
	if s.cfg.KeepAlive != nil {
		poses = append(poses,
			requiredPose{s.filterSw, filterPosePurgeApproach},
			requiredPose{s.filterSw, filterPosePurgePress},
		)
	}
```

- [ ] **Step 8: Verify nothing else broke**

Run: `go build ./... && go test ./coffee`
Expected: PASS. Every existing test must still pass — no existing test sets `KeepAlive`, so `requiredPoses()` returns exactly what it did before.

- [ ] **Step 9: Document the config in README.md**

In `README.md`, in the `viam:beanjamin:coffee` configuration table (the table containing the `button_press_hold_sec` row near line 266), add one row at the end of the table:

```markdown
| `keepalive`                | object | No       | Enables the keep-alive purge loop, which holds the machine's 1 CUP button periodically so the machine never falls out of brew temperature. Requires `has_separate_brew_buttons`. Omit to disable. Fields: `auto_start` and `end` (`"HH:MM"` local, the half-open window `[auto_start, end)`; `auto_start` must mirror the time programmed into the machine's own Auto Start setting), `timezone` (IANA name, required), `days` (lowercase three-letter names, default Mon–Fri), `after_min` (idle minutes before a purge is due, default 40), `check_interval_min` (tick period, default 5), `hold_sec` (button dwell, default 1 — this sets how much water reaches the drip tray). `after_min + 2*check_interval_min` must be under 55, since the machine sleeps after roughly 60 idle minutes. |
```

- [ ] **Step 10: Lint and commit**

```bash
make lint
go build ./...
go test ./coffee
git add coffee/config.go coffee/keepalive.go coffee/espresso.go coffee/keepalive_test.go README.md
git commit -m "Validate keep-alive config and gate its poses

The margin rule is the load-bearing one: the machine sleeps after about
60 idle minutes, so after_min plus two tick periods has to stay under 55
or a single tick skipped by a running order drops the machine to power
save. Pose requirements are gated on keepalive being configured, since
requiring them unconditionally would fail construction on every machine
that will never travel to them."
```

---

### Task 3: Last-activity persistence

The loop needs to know when water last ran through the machine, and that has to survive a reconfigure — otherwise a config-heavy afternoon fires a spurious purge after every reload. `VIAM_MODULE_DATA` is the RDK-provided per-machine, per-module directory; the module manager creates it and it survives restarts, reconfigures, and version upgrades.

The stored value is **last machine activity**, not last brew. A purge is itself activity, so a file tracking only brews would re-fire every tick until somebody ordered a coffee.

**Files:**
- Modify: `coffee/keepalive.go`
- Modify: `coffee/keepalive_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type machineActivityStore struct` with `func (a *machineActivityStore) get() time.Time` and `func (a *machineActivityStore) record(logger logging.Logger, t time.Time)`
  - `func newMachineActivityStore(logger logging.Logger) *machineActivityStore`
  - `const machineActivityFile = "machine-activity"`

- [ ] **Step 1: Write the failing tests**

Append to `coffee/keepalive_test.go`, and add `"os"`, `"path/filepath"`, and `"go.viam.com/rdk/logging"` to its import block:

```go
func TestMachineActivityStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIAM_MODULE_DATA", dir)
	logger := logging.NewTestLogger(t)

	// A fresh store with no file on disk reports the zero time, which makes any
	// elapsed check due — the machine is treated as long idle.
	first := newMachineActivityStore(logger)
	if got := first.get(); !got.IsZero() {
		t.Errorf("get() on an empty store = %v, want the zero time", got)
	}

	stamp := time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC)
	first.record(logger, stamp)
	if got := first.get(); !got.Equal(stamp) {
		t.Errorf("get() after record = %v, want %v", got, stamp)
	}

	// A new store reads the persisted value back — this is the reconfigure case.
	second := newMachineActivityStore(logger)
	if got := second.get(); !got.Equal(stamp) {
		t.Errorf("get() on a reloaded store = %v, want %v", got, stamp)
	}
}

func TestMachineActivityStoreWithoutModuleData(t *testing.T) {
	t.Setenv("VIAM_MODULE_DATA", "")
	logger := logging.NewTestLogger(t)

	// No directory to write to: the store still works in memory, it just does not
	// survive a restart.
	a := newMachineActivityStore(logger)
	stamp := time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC)
	a.record(logger, stamp)
	if got := a.get(); !got.Equal(stamp) {
		t.Errorf("in-memory get() = %v, want %v", got, stamp)
	}
}

func TestMachineActivityStoreCorruptFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIAM_MODULE_DATA", dir)
	logger := logging.NewTestLogger(t)

	if err := os.WriteFile(filepath.Join(dir, machineActivityFile), []byte("not a timestamp"), 0o644); err != nil {
		t.Fatalf("seeding a corrupt file: %v", err)
	}

	// Unreadable content must degrade to "long idle" rather than failing
	// construction — a purge is cheap and a wedged coffee service is not.
	a := newMachineActivityStore(logger)
	if got := a.get(); !got.IsZero() {
		t.Errorf("get() with a corrupt file = %v, want the zero time", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./coffee -run TestMachineActivityStore -v`
Expected: FAIL — `undefined: newMachineActivityStore`, `undefined: machineActivityFile`.

- [ ] **Step 3: Write the implementation**

Append to `coffee/keepalive.go`, adding `"os"`, `"path/filepath"`, `"sync"`, and `"go.viam.com/rdk/logging"` to its import block:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./coffee -run TestMachineActivityStore -v`
Expected: PASS.

- [ ] **Step 5: Lint and commit**

```bash
make lint
go test ./coffee
git add coffee/keepalive.go coffee/keepalive_test.go
git commit -m "Persist last machine activity in VIAM_MODULE_DATA

The value is last *activity*, not last brew: a purge is itself activity,
so tracking only brews would re-fire every tick until someone ordered a
coffee. Persisting matters because the service is AlwaysRebuild — without
it, an afternoon of config edits fires a spurious purge per reload. Every
read failure degrades to 'long idle' rather than failing construction."
```

---

### Task 4: The purge step sequence

Three moves on the filter switch. The collision-scope rule here is the same one already paid for in `brewButtonSteps`, and the test locks it down the same way.

**Files:**
- Modify: `coffee/collisions.go`
- Modify: `coffee/keepalive.go`
- Create: `coffee/keepalive_purge_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `filterPosePurgeApproach`, `filterPosePurgePress` (Task 2); `KeepAlive.hold()` (Task 2).
- Produces:
  - `var filterCoffeeButtonCollisions []AllowedCollision`
  - `func (s *beanjaminCoffee) purgeSteps() []Step`

- [ ] **Step 1: Write the failing test**

Create `coffee/keepalive_purge_test.go`:

```go
package coffee

import "testing"

// TestPurgeStepsCollisionScope locks down which moves of the keep-alive purge may
// carry filterCoffeeButtonCollisions.
//
// An allowed-collision entry applies to the whole plan, not just near its goal.
// The approach is free-planned from home, so allowing coffee-machine-buffer-front
// there lets the planner route the portafilter straight through the machine's
// front face. Only the two short linear moves, which genuinely sit inside that
// obstacle, may allow it. This mirrors TestBrewButtonStepsCollisionScope.
func TestPurgeStepsCollisionScope(t *testing.T) {
	s := &beanjaminCoffee{cfg: &Config{
		HasSeparateBrewButtons: true,
		KeepAlive:              &KeepAlive{AutoStart: "07:45", End: "17:00", Timezone: "UTC"},
	}}
	steps := s.purgeSteps()

	if len(steps) != 3 {
		t.Fatalf("purgeSteps returned %d steps, want 3 (approach, press, retreat)", len(steps))
	}

	tests := []struct {
		name          string
		step          Step
		wantPose      string
		wantLinear    bool
		wantCollision bool
	}{
		{"approach", steps[0], filterPosePurgeApproach, false, false},
		{"press", steps[1], filterPosePurgePress, true, true},
		{"retreat", steps[2], filterPosePurgeApproach, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.step.PoseName != tt.wantPose {
				t.Errorf("PoseName = %q, want %q", tt.step.PoseName, tt.wantPose)
			}
			if gotLinear := tt.step.LinearConstraint != nil; gotLinear != tt.wantLinear {
				t.Errorf("has LinearConstraint = %v, want %v", gotLinear, tt.wantLinear)
			}
			gotCollision := len(tt.step.AllowedCollisions) > 0
			if gotCollision != tt.wantCollision {
				t.Errorf("has AllowedCollisions = %v, want %v — a free-planned move must not "+
					"allow coffee-machine-buffer-front, the allowance covers the whole trajectory",
					gotCollision, tt.wantCollision)
			}
		})
	}

	// Only the press dwells. A dwell on the retreat would hold the arm against the
	// machine face for no reason; a dwell on the approach would do nothing at all.
	if steps[1].Pause != s.cfg.KeepAlive.hold() {
		t.Errorf("press Pause = %v, want the configured hold %v", steps[1].Pause, s.cfg.KeepAlive.hold())
	}
	if steps[0].Pause != 0 || steps[2].Pause != 0 {
		t.Errorf("approach/retreat Pause = %v/%v, want 0", steps[0].Pause, steps[2].Pause)
	}
}

// TestPurgeStepsUseFilterSwitch guards the reason these poses exist on the filter
// switch: the arm holds the portafilter through the whole purge, so the
// portafilter is the frame being positioned. Reading them off the claws switch
// would drive the claw geometry to filter-frame coordinates.
func TestPurgeStepsUseFilterSwitch(t *testing.T) {
	// Two distinct switches so the assertion is about identity, not just
	// non-nil-ness. inject.Switch is the RDK's test double, as inject.Arm is in
	// the maintenancesensor tests; no method on it is called here.
	filterSw := &inject.Switch{}
	clawsSw := &inject.Switch{}
	s := &beanjaminCoffee{
		cfg: &Config{
			HasSeparateBrewButtons: true,
			KeepAlive:              &KeepAlive{AutoStart: "07:45", End: "17:00", Timezone: "UTC"},
		},
		filterSw: filterSw,
		clawsSw:  clawsSw,
	}
	for i, step := range s.purgeSteps() {
		if step.PoseSwitch != filterSw {
			t.Errorf("step %d (%s) reads from the wrong switch, want the filter switch", i, step.PoseName)
		}
	}
}
```

Import block for this file is `"testing"` plus `"go.viam.com/rdk/testutils/inject"`. No hand-rolled fake is needed — no existing test in `coffee/` constructs a pose switch, and `inject.Switch` is the RDK's own test double for the switch API.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./coffee -run TestPurgeSteps -v`
Expected: FAIL — `undefined: purgeSteps`, `undefined: filterCoffeeButtonCollisions`.

- [ ] **Step 3: Add the collision set**

In `coffee/collisions.go`, immediately after `clawCoffeeButtonCollisions`:

```go
// filterCoffeeButtonCollisions permits the portafilter assembly to sit inside
// coffee-machine-buffer-front while the arm presses the machine's 1 CUP button
// with the filter still in the claws (the keep-alive purge, keepalive.go).
// Mirrors clawCoffeeButtonCollisions with the filter frames added, since here the
// portafilter — not the bare claw — is what reaches the machine face.
var filterCoffeeButtonCollisions = []AllowedCollision{
	{Frame1: componentFilter, Frame2: "coffee-machine-buffer-front"},
	{Frame1: "portafilter-handle", Frame2: "coffee-machine-buffer-front"},
	{Frame1: componentClaws, Frame2: "coffee-machine-buffer-front"},
	{Frame1: "gripper:claws", Frame2: "coffee-machine-buffer-front"},
}
```

- [ ] **Step 4: Add `purgeSteps`**

Append to `coffee/keepalive.go`:

```go
// purgeSteps is the three-move hold of the machine's 1 CUP button: free-plan into
// the standoff with the portafilter in hand, straight in and dwell so the pump
// runs, straight back out.
//
// Only the two linear moves carry filterCoffeeButtonCollisions. The button sits
// on the machine face behind coffee-machine-buffer-front, so the press and the
// retreat are inside that obstacle and the planner rejects them without the
// allowance.
//
// The approach must not carry it, for exactly the reason brewButtonSteps' approach
// doesn't: an allowed-collision entry is a property of the whole plan, not of the
// neighbourhood of its goal — buildConstraints hands the planner a
// CollisionSpecification covering the entire trajectory. The approach is
// free-planned from home, and allowing coffee-machine-buffer-front across that
// traverse lets the planner route the portafilter through the machine's front
// face.
func (s *beanjaminCoffee) purgeSteps() []Step {
	hold := s.cfg.KeepAlive.hold()
	return []Step{
		{PoseName: filterPosePurgeApproach, PoseSwitch: s.filterSw},
		{PoseName: filterPosePurgePress, PoseSwitch: s.filterSw, Pause: hold,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterCoffeeButtonCollisions},
		{PoseName: filterPosePurgeApproach, PoseSwitch: s.filterSw,
			LinearConstraint: defaultApproachConstraint, AllowedCollisions: filterCoffeeButtonCollisions},
	}
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./coffee -run TestPurgeSteps -v`
Expected: PASS.

- [ ] **Step 6: Document the poses in README.md**

In `README.md`, in the `viam:beanjamin:coffee` section near line 188 where `has_separate_brew_buttons` explains which claw poses the switcher must carry, add a paragraph after that explanation:

```markdown
When `keepalive` is configured, the **filter** pose switcher must additionally carry `purge_approach` and `purge_press`. These are filter-frame poses, not claw-frame ones: the arm keeps the portafilter in its claws and presses the machine's 1 CUP button with the assembly in hand, so nothing is parked in the group head and no filter basket gets wet. Author `purge_press` so the press is a straight-in linear move from `purge_approach`.
```

- [ ] **Step 7: Lint and commit**

```bash
make lint
go test ./coffee
git add coffee/collisions.go coffee/keepalive.go coffee/keepalive_purge_test.go README.md
git commit -m "Add the keep-alive purge step sequence

Three filter-switch moves: standoff, press-and-dwell, retreat. Only the
two linear moves may allow coffee-machine-buffer-front — an allowance
covers the whole trajectory, so carrying it on the free-planned approach
would let the planner route the portafilter through the machine's front
face. Same trap brewButtonSteps documents, same test shape."
```

---

### Task 5: The purge decision and execution

The decision is a pure function over a snapshot, so every skip reason is testable without a service. Execution is thin: take the same `running` gate an order takes, run the steps, record activity.

**Files:**
- Modify: `coffee/keepalive.go`
- Modify: `coffee/api.go` — the step-label const block
- Modify: `coffee/keepalive_test.go`

**Interfaces:**
- Consumes: `keepAliveWindow.contains` (Task 1), `machineActivityStore` (Task 3), `purgeSteps` (Task 4).
- Produces:
  - `type keepAliveState struct { now, lastActivity time.Time; busy, paused bool; queued int }`
  - `func shouldPurge(w *keepAliveWindow, threshold time.Duration, st keepAliveState) (bool, string)`
  - `func (s *beanjaminCoffee) runPurge(ctx context.Context) error`
  - `func (s *beanjaminCoffee) recordMachineActivity()`
  - `func (s *beanjaminCoffee) notifyKeepAliveFailureSlack(purgeErr error)`
  - `const stepKeepAlive = "Keep-alive purge"`
  - Field on `beanjaminCoffee`: `machineActivity *machineActivityStore`

- [ ] **Step 1: Write the failing test**

Append to `coffee/keepalive_test.go`:

```go
func TestShouldPurge(t *testing.T) {
	w, err := newKeepAliveWindow(validKeepAlive()) // 07:45–17:00 America/New_York, Mon–Fri
	if err != nil {
		t.Fatalf("newKeepAliveWindow: %v", err)
	}
	threshold := 40 * time.Minute

	// A Thursday inside the window.
	inWindow := time.Date(2026, 1, 15, 12, 0, 0, 0, w.loc)
	longIdle := inWindow.Add(-90 * time.Minute)
	recent := inWindow.Add(-5 * time.Minute)

	tests := []struct {
		name string
		st   keepAliveState
		want bool
	}{
		{
			name: "idle inside the window purges",
			st:   keepAliveState{now: inWindow, lastActivity: longIdle},
			want: true,
		},
		{
			name: "never used purges",
			st:   keepAliveState{now: inWindow},
			want: true,
		},
		{
			name: "outside the window never purges",
			st:   keepAliveState{now: time.Date(2026, 1, 15, 3, 0, 0, 0, w.loc), lastActivity: longIdle},
			want: false,
		},
		{
			name: "weekend never purges",
			st:   keepAliveState{now: time.Date(2026, 1, 17, 12, 0, 0, 0, w.loc), lastActivity: longIdle},
			want: false,
		},
		{
			name: "a running sequence defers",
			st:   keepAliveState{now: inWindow, lastActivity: longIdle, busy: true},
			want: false,
		},
		{
			name: "a paused queue defers",
			st:   keepAliveState{now: inWindow, lastActivity: longIdle, paused: true},
			want: false,
		},
		{
			name: "queued orders defer — one is about to reset the timer anyway",
			st:   keepAliveState{now: inWindow, lastActivity: longIdle, queued: 1},
			want: false,
		},
		{
			name: "recent use defers",
			st:   keepAliveState{now: inWindow, lastActivity: recent},
			want: false,
		},
		{
			name: "exactly at the threshold purges",
			st:   keepAliveState{now: inWindow, lastActivity: inWindow.Add(-threshold)},
			want: true,
		},
		{
			name: "one second under the threshold defers",
			st:   keepAliveState{now: inWindow, lastActivity: inWindow.Add(-threshold + time.Second)},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := shouldPurge(w, threshold, tt.st)
			if got != tt.want {
				t.Errorf("shouldPurge() = %v (%q), want %v", got, why, tt.want)
			}
			if !got && why == "" {
				t.Error("shouldPurge() declined without giving a reason; the reason is logged")
			}
			if got && why != "" {
				t.Errorf("shouldPurge() approved but gave reason %q, want empty", why)
			}
		})
	}
}

func TestRecordMachineActivityWithoutKeepAlive(t *testing.T) {
	// Every machine without keepalive configured has a nil store, and prepareDrink
	// calls this unconditionally after a successful brew.
	s := &beanjaminCoffee{cfg: &Config{}, logger: logging.NewTestLogger(t)}
	s.recordMachineActivity() // must not panic
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./coffee -run 'TestShouldPurge|TestRecordMachineActivity' -v`
Expected: FAIL — `undefined: keepAliveState`, `undefined: shouldPurge`, `s.recordMachineActivity undefined`.

- [ ] **Step 3: Add the step label**

In `coffee/api.go`, in the step-label const block, after `stepRecoveringFilter`:

```go
	// stepKeepAlive is published while a keep-alive purge runs. No order is
	// active, so it surfaces through Status/get_queue only, never on an order.
	stepKeepAlive = "Keep-alive purge"
```

- [ ] **Step 4: Add the service field**

In `coffee/module.go`, in the `beanjaminCoffee` struct, after the `usageSensor` field:

```go
	// machineActivity tracks when water last ran through the espresso machine, so
	// the keep-alive loop knows whether the machine is about to fall out of brew
	// temperature (keepalive.go). nil when keepalive is unconfigured, which makes
	// recordMachineActivity a no-op.
	machineActivity *machineActivityStore
```

- [ ] **Step 5: Write the decision and execution**

Append to `coffee/keepalive.go`, adding `"context"` and `"errors"` to its import block:

```go
// keepAlivePurgeTimeout bounds one purge. Generous next to three short moves; if
// it is ever hit, something is wedged and the loop must not keep holding the
// `running` flag that the order queue waits on.
const keepAlivePurgeTimeout = 2 * time.Minute

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

// runPurge holds the machine's 1 CUP button so the pump runs and the machine's
// idle timer resets.
//
// It takes the same `running` compare-and-swap that prepareDrink takes, so to the
// order queue a purge is indistinguishable from an order: one arriving mid-purge
// waits rather than planning against an arm that is already moving. Releasing
// that flag on every path is load-bearing — a purge that returned while holding
// it would stall the queue permanently, which is also why the whole thing runs
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
	return nil
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
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./coffee -run 'TestShouldPurge|TestRecordMachineActivity' -v`
Expected: PASS.

- [ ] **Step 7: Lint and commit**

```bash
make lint
go build ./...
go test ./coffee
git add coffee/keepalive.go coffee/api.go coffee/module.go coffee/keepalive_test.go
git commit -m "Add the keep-alive purge decision and execution

shouldPurge is pure over a snapshot so every skip reason is testable and
every skip is logged with its reason — a silent loop is indistinguishable
from a broken one. runPurge takes the same running CAS an order takes, so
the queue treats a purge exactly like an order, and releases it on every
path under a timeout: holding that flag would stall the queue for good."
```

---

### Task 6: The loop, wiring, and recording brews

The last piece: start the goroutine when configured, and record activity when a brew actually pours.

**Files:**
- Modify: `coffee/keepalive.go`
- Modify: `coffee/module.go` — `NewCoffee`
- Modify: `coffee/espresso.go` — `prepareDrink`
- Modify: `README.md`

**Interfaces:**
- Consumes: everything from Tasks 1–5.
- Produces: `func (s *beanjaminCoffee) keepAliveLoop(w *keepAliveWindow)`

- [ ] **Step 1: Write the loop**

Append to `coffee/keepalive.go`:

```go
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
		s.recordMachineActivity()
	}
}
```

- [ ] **Step 2: Wire it into `NewCoffee`**

In `coffee/module.go`, in `NewCoffee`, replace:

```go
	go s.processQueue()
	return s, nil
```

with:

```go
	// Keep the machine at brew temperature during working hours. Started after
	// pose validation so a misconfigured purge pose fails construction rather than
	// surfacing as a failed tick an hour later.
	if conf.KeepAlive != nil {
		window, err := newKeepAliveWindow(conf.KeepAlive)
		if err != nil {
			cancelFunc()
			return nil, fmt.Errorf("keepalive: %w", err)
		}
		s.machineActivity = newMachineActivityStore(logger)
		go s.keepAliveLoop(window)
	}

	go s.processQueue()
	return s, nil
```

- [ ] **Step 3: Record activity when a brew pours**

In `coffee/espresso.go`, in `prepareDrink`, inside the `stepBrewing` block, immediately after the two existing `incrementSensorReading` calls and before the closing brace:

```go
		// The pour reset the machine's own idle timer, so the keep-alive loop's
		// clock restarts here. Keyed off the brew rather than the order: a shot
		// that poured and then failed at serving still counts as machine use.
		s.recordMachineActivity()
```

- [ ] **Step 4: Verify the build and the full suite**

Run: `go build ./... && make test`
Expected: PASS. No existing test configures `KeepAlive`, so the loop never starts in tests and `recordMachineActivity` is a no-op on the nil store.

- [ ] **Step 5: Document the operator setup in README.md**

In `README.md`, at the end of the `viam:beanjamin:coffee` model section (immediately before the `## Model: viam:beanjamin:dial-control-motion` heading near line 504), add:

```markdown
### Keeping the machine at brew temperature

The Breville BES920 drops into POWER SAVE after one hour idle and powers off completely after four, and the one-hour sleep cannot be disabled in its settings. A brew started on a sleeping machine is refused with three beeps, so the arm serves an empty cup and records it as a success. Two things prevent that, and both are needed:

1. **Program the machine's own Auto Start.** `MENU` → `AUTO START` → `ON` → a time ~15 minutes before people arrive. This handles the cold morning in hardware, and the same time goes in `keepalive.auto_start`. Note Auto Start has no day-of-week setting, so it also fires at weekends; the machine's own Auto Off shuts it down again after four hours.
2. **Configure `keepalive`.** During the window, the arm holds the 1 CUP button for about a second whenever nothing has run water through the machine for `after_min`. This is Breville's documented group-head purge, and it resets the machine's idle timer so it never leaves brew temperature.

The arm never presses POWER — per the manual, pressing POWER while the machine is in POWER SAVE turns it *off*. The consequence is that this cannot recover a machine that is genuinely powered down: if Auto Start does not fire, or someone switches the machine off, every order that day will brew cold and be recorded as a success. Detecting that needs a machine-state sensor, which is not part of this feature.

Water from each purge goes to the drip tray and is counted in the `drip_tray_brews` usage-sensor field, so empty the tray on the counter rather than on brew count alone.
```

- [ ] **Step 6: Lint and commit**

```bash
make lint
go build ./...
make test
git add coffee/keepalive.go coffee/module.go coffee/espresso.go README.md
git commit -m "Start the keep-alive loop and record brews as machine activity

The loop watches queueStop rather than cancelCtx: an operator cancel
pauses the queue instead of shutting the service down, and shouldPurge
already declines while paused, so the loop resumes on its own after
'proceed'. Activity is recorded off a successful brew rather than a
successful order, since a shot that poured and then failed at serving
still reset the machine's timer."
```

---

## Post-implementation verification

These need the physical machine and are not code tasks. They are the tuning the spec flags as unresolved.

- [ ] Author `purge_approach` and `purge_press` on the filter pose switcher with `viam robot part motion get-pose` / `set-pose`, confirm the press is a straight-in linear move from the standoff, and commit them to the switcher config only after verifying physically.
- [ ] Confirm the planner accepts the press and retreat with `filterCoffeeButtonCollisions`, and trim any of the four pairs it does not need.
- [ ] Measure the water one `hold_sec: 1.0` purge sends to the drip tray; adjust `hold_sec` and the tray-emptying interval.
- [ ] Time the machine's real sleep onset (owners report 45–60 minutes) and set `after_min` with the margin rule in mind.
- [ ] Optional, would remove water from the design entirely: leave the machine at STANDBY, tap `UP` at ~55 minutes, and check at ~70 whether it stayed awake. If a bare button press resets the idle timer, the purge becomes a zero-water arrow tap — only the pose and `hold_sec` change.
