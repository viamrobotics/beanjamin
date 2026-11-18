package refilldisplay

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
)

// fakeMonitor is a sensor whose readings the test sets.
type fakeMonitor struct {
	resource.Named
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	mu       sync.Mutex
	readings map[string]any
	err      error
	doErr    error
	doCmds   []map[string]any
}

func newFakeMonitor(readings map[string]any) *fakeMonitor {
	return &fakeMonitor{Named: resource.NewName(sensor.API, "monitor").AsNamed(), readings: readings}
}

func (f *fakeMonitor) set(readings map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readings = readings
}

func (f *fakeMonitor) Readings(context.Context, map[string]any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.readings, nil
}

func (f *fakeMonitor) DoCommand(_ context.Context, cmd map[string]any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.doCmds = append(f.doCmds, cmd)
	if f.doErr != nil {
		return nil, f.doErr
	}
	return map[string]any{}, nil
}

func (f *fakeMonitor) commands() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.doCmds...)
}

func (f *fakeMonitor) failCommands(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.doErr = err
}

// fakeDeck records every update_display it receives.
type fakeDeck struct {
	resource.Named
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	mu    sync.Mutex
	calls []map[string]any
	err   error
}

func newFakeDeck() *fakeDeck {
	return &fakeDeck{Named: resource.NewName(generic.API, "deck").AsNamed()}
}

func (f *fakeDeck) DoCommand(_ context.Context, cmd map[string]any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.calls = append(f.calls, cmd)
	return map[string]any{}, nil
}

func (f *fakeDeck) sent() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.calls...)
}

// keysOf returns the "keys" map of the n-th update_display call.
func keysOf(t *testing.T, calls []map[string]any, n int) map[string]any {
	t.Helper()
	if n >= len(calls) {
		t.Fatalf("only %d calls, wanted index %d", len(calls), n)
	}
	upd, ok := calls[n]["update_display"].(map[string]any)
	if !ok {
		t.Fatalf("call %d is not update_display: %v", n, calls[n])
	}
	return upd["keys"].(map[string]any)
}

func newTestDisplay(t *testing.T, mon sensor.Sensor, deck resource.Resource, page string) (*refillDisplay, *time.Time) {
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	r := &refillDisplay{
		name:    generic.Named("refill-display"),
		logger:  logging.NewTestLogger(t),
		monitor: mon,
		usage:   newFakeMonitor(nil),
		deck:    deck,
		page:    page,
		buttons: []Button{
			{Name: "water", Reading: "water_used", Label: "Water", Threshold: 25},
			{Name: "beans", Reading: "regular_grinds", Label: "Beans", Threshold: 20, Text: "{label}: {value} uses"},
		},
		colors: Colors{}.withDefaults(),
		now:    func() time.Time { return clock },
		pushed: map[string]painted{},
	}
	return r, &clock
}

func TestStateOf(t *testing.T) {
	cases := []struct {
		value     float64
		triggered bool
		near      bool
		want      state
	}{
		{0, false, false, stateFresh},
		{3, false, false, stateOK},
		{22, false, true, stateNear},
		{25, true, true, stateDue},
		{30, true, false, stateDue},
		{0, true, false, stateDue}, // the monitor's rule is the authority
		{0, false, true, stateFresh},
	}
	for _, c := range cases {
		if got := stateOf(c.value, c.triggered, c.near); got != c.want {
			t.Errorf("stateOf(%v, triggered=%v, near=%v) = %q, want %q", c.value, c.triggered, c.near, got, c.want)
		}
	}
}

func TestRender(t *testing.T) {
	colors := Colors{}.withDefaults()
	b := Button{Name: "water", Reading: "water_used", Label: "Water", Threshold: 25}

	p, ok := render(b, map[string]any{"water_used": 0.0, "water_used_triggered": false}, colors, 0)
	if !ok || p.Color != "green" || p.Text != "Water 0/25" || p.State != stateFresh {
		t.Errorf("fresh: got %+v ok=%v", p, ok)
	}
	p, _ = render(b, map[string]any{"water_used": 4.5, "water_used_triggered": false}, colors, 0)
	if p.Color != "teal" || p.Text != "Water 4.5/25" {
		t.Errorf("ok: got %+v", p)
	}
	p, _ = render(b, map[string]any{"water_used": 25.0, "water_used_triggered": true}, colors, 0)
	if p.Color != "red" || p.State != stateDue {
		t.Errorf("due: got %+v", p)
	}

	// No rule for the reading means no _triggered flag, so it can never be due,
	// and with no threshold the text shows just the count.
	p, ok = render(Button{Name: "water", Reading: "water_used", Label: "Water"}, map[string]any{"water_used": 99.0}, colors, 0)
	if !ok || p.Color != "teal" || p.Text != "Water 99" {
		t.Errorf("no rule: got %+v ok=%v", p, ok)
	}

	// {threshold} in custom text is "?" when the button has none.
	p, _ = render(Button{Name: "ice", Reading: "ice_dispenses", Text: "{value} of {threshold}"}, map[string]any{"ice_dispenses": 2.0}, colors, 0)
	if p.Text != "2 of ?" {
		t.Errorf("unknown threshold: got %q", p.Text)
	}

	// Name stands in for a missing label, and custom text and colors apply.
	p, _ = render(Button{Name: "ice", Reading: "ice_dispenses", Text: "Used {value}x"}, map[string]any{"ice_dispenses": 2.0}, Colors{OK: "teal"}.withDefaults(), 0)
	if p.Text != "Used 2x" || p.Color != "teal" {
		t.Errorf("custom: got %+v", p)
	}

	for _, bad := range []any{nil, "12", true} {
		if _, ok := render(b, map[string]any{"water_used": bad}, colors, 0); ok {
			t.Errorf("reading %#v should not render", bad)
		}
	}
	if _, ok := render(b, map[string]any{}, colors, 0); ok {
		t.Error("missing reading should not render")
	}
}

// Outside a rule's active windows the monitor freezes "<key>_triggered", so the
// display must judge by the count, not the stale flag.
func TestRenderWhenMonitorIsSnoozed(t *testing.T) {
	colors := Colors{}.withDefaults()
	b := Button{Name: "water", Reading: "water_used", Label: "Water", Threshold: 25}

	// Refilled on a weekend: the flag is still true from Friday, the count is 0.
	p, _ := render(b, map[string]any{"water_used": 0.0, "water_used_triggered": true, "water_used_snoozed": true}, colors, 0)
	if p.State != stateFresh || p.Color != "green" {
		t.Errorf("stale flag after a refill: got %+v, want fresh/green", p)
	}

	// Ran down overnight: the flag is still false, the count is past the limit.
	p, _ = render(b, map[string]any{"water_used": 26.0, "water_used_triggered": false, "water_used_snoozed": true}, colors, 0)
	if p.State != stateDue || p.Color != "red" {
		t.Errorf("past the limit while snoozed: got %+v, want due/red", p)
	}

	// Within the limit while snoozed.
	p, _ = render(b, map[string]any{"water_used": 10.0, "water_used_triggered": true, "water_used_snoozed": true}, colors, 0)
	if p.State != stateOK {
		t.Errorf("within the limit while snoozed: got %+v, want ok", p)
	}

	// Snoozed with no threshold known: nothing better than the flag.
	p, _ = render(Button{Name: "water", Reading: "water_used", Label: "Water"}, map[string]any{"water_used": 0.0, "water_used_triggered": true, "water_used_snoozed": true}, colors, 0)
	if p.State != stateDue {
		t.Errorf("snoozed without a threshold keeps the flag: got %+v", p)
	}

	// Not snoozed: the monitor's flag is the authority, as before.
	p, _ = render(b, map[string]any{"water_used": 10.0, "water_used_triggered": true, "water_used_snoozed": false}, colors, 0)
	if p.State != stateDue {
		t.Errorf("not snoozed: got %+v, want the flag to win", p)
	}
}

func TestIsNear(t *testing.T) {
	cases := []struct {
		value, threshold, pct float64
		want                  bool
	}{
		{20, 25, 20, true},  // exactly on the line
		{19, 25, 20, false}, // just short of it
		{24, 25, 20, true},
		{16, 20, 20, true},
		{15, 20, 20, false},
		{5, 6, 20, true}, // 4.8 and up
		{4, 6, 20, false},
		{1, 25, 0, false},  // 0 turns the warning off
		{25, 25, 20, true}, // at the threshold; the monitor decides "due"
		{87.5, 100, 12.5, true},
	}
	for _, c := range cases {
		if got := isNear(c.value, c.threshold, c.pct); got != c.want {
			t.Errorf("isNear(%v, %v, %v%%) = %v, want %v", c.value, c.threshold, c.pct, got, c.want)
		}
	}
}

func TestRenderNearBand(t *testing.T) {
	colors := Colors{Near: "lightsalmon"}.withDefaults()
	b := Button{Name: "water", Reading: "water_used", Label: "Water", Threshold: 25}
	reading := func(v float64, triggered bool) map[string]any {
		return map[string]any{"water_used": v, "water_used_triggered": triggered}
	}

	p, _ := render(b, reading(20, false), colors, 20)
	if p.State != stateNear || p.Color != "lightsalmon" || p.Text != "Water 20/25" {
		t.Errorf("on the line: got %+v", p)
	}
	p, _ = render(b, reading(19, false), colors, 20)
	if p.State != stateOK {
		t.Errorf("below the band: got %+v", p)
	}
	// Due and fresh both win over the warning.
	p, _ = render(b, reading(25, true), colors, 20)
	if p.State != stateDue || p.Color != "red" {
		t.Errorf("due inside the band: got %+v", p)
	}
	p, _ = render(b, reading(0, false), colors, 20)
	if p.State != stateFresh {
		t.Errorf("fresh: got %+v", p)
	}

	// Off by default.
	p, _ = render(b, reading(24, false), colors, 0)
	if p.State != stateOK {
		t.Errorf("near_percent unset: got %+v, want ok", p)
	}

	// No threshold, no warning.
	p, _ = render(Button{Name: "water", Reading: "water_used", Label: "Water"}, map[string]any{"water_used": 24.0}, colors, 20)
	if p.State != stateOK {
		t.Errorf("no threshold: got %+v, want ok", p)
	}

	// While snoozed the count decides, and the band still applies.
	p, _ = render(b, map[string]any{"water_used": 21.0, "water_used_triggered": true, "water_used_snoozed": true}, colors, 20)
	if p.State != stateNear {
		t.Errorf("snoozed within the band with a stale flag: got %+v, want near", p)
	}

	// The default near color applies if none is configured.
	p, _ = render(b, reading(22, false), Colors{}.withDefaults(), 20)
	if p.Color != "salmon" {
		t.Errorf("default near color = %q, want salmon", p.Color)
	}
}

func TestValidate(t *testing.T) {
	good := Config{
		MonitorSensorName: "monitor-sensor",
		UsageSensorName:   "usage-sensor",
		StreamDeckName:    "stream-deck",
		Buttons:           []Button{{Name: "water", Reading: "water_used"}},
	}
	deps, _, err := good.Validate("p")
	if err != nil {
		t.Fatalf("good config rejected: %v", err)
	}
	if len(deps) != 3 {
		t.Errorf("deps = %v, want the monitor, the usage sensor and the deck", deps)
	}

	bad := map[string]func(*Config){
		"no monitor":       func(c *Config) { c.MonitorSensorName = "" },
		"no usage sensor":  func(c *Config) { c.UsageSensorName = "" },
		"no deck":          func(c *Config) { c.StreamDeckName = "" },
		"no buttons":       func(c *Config) { c.Buttons = nil },
		"no button name":   func(c *Config) { c.Buttons[0].Name = "" },
		"numeric name":     func(c *Config) { c.Buttons[0].Name = "4" },
		"no reading":       func(c *Config) { c.Buttons[0].Reading = "" },
		"duplicate name":   func(c *Config) { c.Buttons = append(c.Buttons, Button{Name: "water", Reading: "x"}) },
		"negative polling": func(c *Config) { c.PollIntervalSeconds = -1 },
		"negative near":    func(c *Config) { c.NearPercent = -5 },
		"near of 100":      func(c *Config) { c.NearPercent = 100 },
		"negative limit":   func(c *Config) { c.Buttons[0].Threshold = -1 },
	}
	for name, mutate := range bad {
		c := good
		c.Buttons = append([]Button(nil), good.Buttons...)
		mutate(&c)
		if _, _, err := c.Validate("p"); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSyncPushesOnlyChanges(t *testing.T) {
	mon := newFakeMonitor(map[string]any{
		"water_used": 3.0, "water_used_triggered": false,
		"regular_grinds": 0.0, "regular_grinds_triggered": false,
	})
	deck := newFakeDeck()
	r, _ := newTestDisplay(t, mon, deck, "Refill")
	ctx := context.Background()

	// First sync paints both buttons, on the configured page.
	if err := r.sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	calls := deck.sent()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	upd := calls[0]["update_display"].(map[string]any)
	if upd["page"] != "Refill" {
		t.Errorf("page = %v, want Refill", upd["page"])
	}
	keys := keysOf(t, calls, 0)
	water := keys["water"].(map[string]any)
	beans := keys["beans"].(map[string]any)
	if water["text"] != "Water 3/25" || water["color"] != "teal" {
		t.Errorf("water = %v", water)
	}
	if beans["text"] != "Beans: 0 uses" || beans["color"] != "green" {
		t.Errorf("beans = %v", beans)
	}

	// Nothing changed, so nothing is sent.
	if err := r.sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if n := len(deck.sent()); n != 1 {
		t.Errorf("calls = %d after an unchanged poll, want 1", n)
	}

	// One counter crosses its threshold: only that button is sent.
	mon.set(map[string]any{
		"water_used": 25.0, "water_used_triggered": true,
		"regular_grinds": 0.0, "regular_grinds_triggered": false,
	})
	if err := r.sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	calls = deck.sent()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	keys = keysOf(t, calls, 1)
	if len(keys) != 1 {
		t.Errorf("sent %d buttons, want only water: %v", len(keys), keys)
	}
	if keys["water"].(map[string]any)["color"] != "red" {
		t.Errorf("water not red: %v", keys["water"])
	}

	// A refill resets it to fresh/green.
	mon.set(map[string]any{
		"water_used": 0.0, "water_used_triggered": false,
		"regular_grinds": 0.0, "regular_grinds_triggered": false,
	})
	if err := r.sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	keys = keysOf(t, deck.sent(), 2)
	if keys["water"].(map[string]any)["color"] != "green" {
		t.Errorf("water not green after refill: %v", keys["water"])
	}
}

func TestSyncWithoutPageOmitsIt(t *testing.T) {
	mon := newFakeMonitor(map[string]any{"water_used": 1.0, "regular_grinds": 1.0})
	deck := newFakeDeck()
	r, _ := newTestDisplay(t, mon, deck, "")
	if err := r.sync(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	upd := deck.sent()[0]["update_display"].(map[string]any)
	if _, has := upd["page"]; has {
		t.Errorf("page should be omitted when not configured: %v", upd)
	}
}

func TestSyncRepaintsEverythingPeriodically(t *testing.T) {
	mon := newFakeMonitor(map[string]any{"water_used": 3.0, "regular_grinds": 1.0})
	deck := newFakeDeck()
	r, clock := newTestDisplay(t, mon, deck, "Refill")
	ctx := context.Background()

	_ = r.sync(ctx, false)
	_ = r.sync(ctx, false)
	if n := len(deck.sent()); n != 1 {
		t.Fatalf("calls = %d, want 1 before the repaint interval", n)
	}

	*clock = clock.Add(repaintEvery + time.Second)
	_ = r.sync(ctx, false)
	calls := deck.sent()
	if len(calls) != 2 || len(keysOf(t, calls, 1)) != 2 {
		t.Errorf("expected a full repaint of both buttons, got %v", calls)
	}
}

func TestSyncRetriesWhenDeckIsDown(t *testing.T) {
	mon := newFakeMonitor(map[string]any{"water_used": 3.0, "regular_grinds": 1.0})
	deck := newFakeDeck()
	deck.err = errors.New("no streamdeck found")
	r, _ := newTestDisplay(t, mon, deck, "Refill")
	ctx := context.Background()

	if err := r.sync(ctx, false); err == nil {
		t.Fatal("expected the deck error")
	}
	if len(r.pushed) != 0 {
		t.Fatalf("nothing should be recorded as sent: %v", r.pushed)
	}

	// When the deck comes back the same buttons are sent again.
	deck.mu.Lock()
	deck.err = nil
	deck.mu.Unlock()
	if err := r.sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if n := len(deck.sent()); n != 1 {
		t.Errorf("calls = %d, want the retry to send once", n)
	}
}

func TestSyncMonitorErrorAndMissingReading(t *testing.T) {
	mon := newFakeMonitor(map[string]any{"water_used": 3.0}) // regular_grinds is missing
	deck := newFakeDeck()
	r, _ := newTestDisplay(t, mon, deck, "Refill")
	ctx := context.Background()

	// A missing reading skips that button but still paints the others.
	if err := r.sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	keys := keysOf(t, deck.sent(), 0)
	if _, ok := keys["beans"]; ok || keys["water"] == nil {
		t.Errorf("expected only water, got %v", keys)
	}

	mon.mu.Lock()
	mon.err = errors.New("boom")
	mon.mu.Unlock()
	if err := r.sync(ctx, false); err == nil {
		t.Error("expected the monitor error")
	}
	if n := len(deck.sent()); n != 1 {
		t.Errorf("calls = %d, a monitor failure must not touch the deck", n)
	}
}

func TestDoCommandRefresh(t *testing.T) {
	mon := newFakeMonitor(map[string]any{"water_used": 3.0, "regular_grinds": 0.0})
	deck := newFakeDeck()
	r, _ := newTestDisplay(t, mon, deck, "Refill")
	ctx := context.Background()

	res, err := r.DoCommand(ctx, map[string]any{"refresh": true})
	if err != nil {
		t.Fatal(err)
	}
	buttons := res["buttons"].(map[string]any)
	if len(buttons) != 2 {
		t.Errorf("buttons = %v", buttons)
	}

	// A refresh repaints everything even if nothing changed.
	if _, err := r.DoCommand(ctx, map[string]any{"refresh": true}); err != nil {
		t.Fatal(err)
	}
	if n := len(deck.sent()); n != 2 {
		t.Errorf("calls = %d, want a repaint on each refresh", n)
	}

	if _, err := r.DoCommand(ctx, map[string]any{"nope": true}); err == nil {
		t.Error("expected an error for an unknown command")
	}
}

func TestRefillResetsCounterAndRepaints(t *testing.T) {
	mon := newFakeMonitor(map[string]any{
		"water_used": 25.0, "water_used_triggered": true,
		"regular_grinds": 4.0, "regular_grinds_triggered": false,
	})
	deck := newFakeDeck()
	r, _ := newTestDisplay(t, mon, deck, "Refill")
	usage := r.usage.(*fakeMonitor)
	ctx := context.Background()
	_ = r.sync(ctx, false) // the deck shows the starting state

	// The monitor re-checks and sees the reset counter once asked to.
	mon.set(map[string]any{
		"water_used": 0.0, "water_used_triggered": false,
		"regular_grinds": 4.0, "regular_grinds_triggered": false,
	})
	res, err := r.DoCommand(ctx, map[string]any{"refill": "water"})
	if err != nil {
		t.Fatal(err)
	}
	if res["refill"] != "water" || res["reading"] != "water_used" || res["refreshed"] != true {
		t.Errorf("result = %v", res)
	}

	// Only that button's counter was reset, on the usage sensor.
	cmds := usage.commands()
	if len(cmds) != 1 {
		t.Fatalf("usage commands = %v, want 1", cmds)
	}
	merge, _ := cmds[0]["merge"].(map[string]any)
	if len(merge) != 1 || merge["water_used"] != 0.0 {
		t.Errorf("merge = %v, want only usage: 0", cmds[0])
	}

	// The monitor was asked to re-check, and the deck repainted green.
	if cmds := mon.commands(); len(cmds) != 1 || cmds[0]["check"] != true {
		t.Errorf("monitor commands = %v, want a check", cmds)
	}
	calls := deck.sent()
	keys := keysOf(t, calls, len(calls)-1)
	if keys["water"].(map[string]any)["color"] != "green" {
		t.Errorf("water not repainted green: %v", keys)
	}
}

func TestRefillRejectsBadInput(t *testing.T) {
	mon := newFakeMonitor(map[string]any{"water_used": 1.0})
	r, _ := newTestDisplay(t, mon, newFakeDeck(), "Refill")
	usage := r.usage.(*fakeMonitor)
	ctx := context.Background()

	for _, bad := range []any{"ghost", "", 3, nil} {
		if _, err := r.DoCommand(ctx, map[string]any{"refill": bad}); err == nil {
			t.Errorf("refill %#v: expected an error", bad)
		}
	}
	if n := len(usage.commands()) + len(mon.commands()); n != 0 {
		t.Errorf("a bad refill must not touch any sensor, saw %d commands", n)
	}
}

func TestRefillResetFailureIsAnError(t *testing.T) {
	mon := newFakeMonitor(map[string]any{"water_used": 5.0})
	r, _ := newTestDisplay(t, mon, newFakeDeck(), "Refill")
	r.usage.(*fakeMonitor).failCommands(errors.New("sensor down"))

	if _, err := r.DoCommand(context.Background(), map[string]any{"refill": "water"}); err == nil {
		t.Fatal("expected the reset failure to be returned")
	}
	if n := len(mon.commands()); n != 0 {
		t.Errorf("the monitor must not be asked to re-check after a failed reset, saw %d", n)
	}
}

// The counter is the part that matters: if only the follow-up fails it is still
// reset, so the call succeeds and reports that the buttons will catch up later.
func TestRefillSucceedsWhenRecheckFails(t *testing.T) {
	mon := newFakeMonitor(map[string]any{"water_used": 5.0, "regular_grinds": 1.0})
	mon.failCommands(errors.New("monitor busy"))
	r, _ := newTestDisplay(t, mon, newFakeDeck(), "Refill")
	usage := r.usage.(*fakeMonitor)

	res, err := r.DoCommand(context.Background(), map[string]any{"refill": "water"})
	if err != nil {
		t.Fatalf("a failed re-check must not fail the refill: %v", err)
	}
	if res["refreshed"] != false {
		t.Errorf("refreshed = %v, want false", res["refreshed"])
	}
	if n := len(usage.commands()); n != 1 {
		t.Errorf("the counter should still have been reset, saw %d usage commands", n)
	}
}
