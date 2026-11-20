package coffee

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	genericcomponent "go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// fakeLCD records the DoCommands a character-lcd display receives.
type fakeLCD struct {
	resource.Named
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	mu   sync.Mutex
	cmds []map[string]any
}

func newFakeLCD(name string) *fakeLCD {
	return &fakeLCD{Named: genericcomponent.Named(name).AsNamed()}
}

func (f *fakeLCD) DoCommand(_ context.Context, cmd map[string]any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, cmd)
	return map[string]any{}, nil
}

func (f *fakeLCD) commands() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.cmds...)
}

// waitForCommands polls until lcd has received n commands, then returns them.
func waitForCommands(t *testing.T, lcd *fakeLCD, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		cmds := lcd.commands()
		if len(cmds) >= n {
			return cmds
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d display commands %v, want %d", len(cmds), cmds, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var (
	lcdBacklightOn  = map[string]any{"command": "backlight", "on": true}
	lcdBacklightOff = map[string]any{"command": "backlight", "on": false}
	lcdClear        = map[string]any{"command": "clear"}
)

func lcdLines(customer, drink string) map[string]any {
	return map[string]any{"command": "write_lines", "lines": []any{customer, drink}}
}

func TestSlotDisplaysShowThenClear(t *testing.T) {
	slot0, slot1 := newFakeLCD("lcd-0"), newFakeLCD("lcd-1")
	deps := resource.Dependencies{slot0.Name(): slot0, slot1.Name(): slot1}
	sd := newSlotDisplays(deps, []string{"lcd-0", "lcd-1"}, logging.NewTestLogger(t))
	defer sd.close()
	sd.clearAfter = 50 * time.Millisecond

	// Both displays are blanked on startup.
	for _, lcd := range []*fakeLCD{slot0, slot1} {
		if got, want := waitForCommands(t, lcd, 2), []map[string]any{lcdClear, lcdBacklightOff}; !reflect.DeepEqual(got, want) {
			t.Fatalf("startup commands = %v, want %v", got, want)
		}
	}

	sd.show(1, "Ada", "latte")
	got := waitForCommands(t, slot1, 6)
	want := []map[string]any{lcdClear, lcdBacklightOff, lcdBacklightOn, lcdLines("Ada", "latte"), lcdClear, lcdBacklightOff}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("slot 1 commands = %v, want %v", got, want)
	}
	if n := len(slot0.commands()); n != 2 {
		t.Fatalf("slot 0 got %d commands, want only the 2 startup ones", n)
	}
}

func TestSlotDisplaysNewerShowKeepsItsText(t *testing.T) {
	lcd := newFakeLCD("lcd-0")
	sd := newSlotDisplays(resource.Dependencies{lcd.Name(): lcd}, []string{"lcd-0"}, logging.NewTestLogger(t))
	defer sd.close()
	waitForCommands(t, lcd, 2)

	sd.clearAfter = 100 * time.Millisecond
	sd.show(0, "Ada", "latte")
	waitForCommands(t, lcd, 4)
	sd.clearAfter = time.Hour
	sd.show(0, "Bob", "espresso")
	waitForCommands(t, lcd, 6)

	// The first show's clear is due now; it must not blank Bob's drink.
	time.Sleep(200 * time.Millisecond)
	got := lcd.commands()
	if len(got) != 6 || !reflect.DeepEqual(got[5], lcdLines("Bob", "espresso")) {
		t.Fatalf("commands = %v, want Bob's text left up", got)
	}
}

func TestSlotDisplaysIgnoresUnmappedSlots(t *testing.T) {
	lcd := newFakeLCD("lcd-0")
	// "lcd-1" is configured but not in deps (its sub-part is offline).
	sd := newSlotDisplays(resource.Dependencies{lcd.Name(): lcd}, []string{"lcd-0", "lcd-1"}, logging.NewTestLogger(t))
	defer sd.close()
	waitForCommands(t, lcd, 2)

	sd.show(1, "Ada", "latte")
	sd.show(2, "Ada", "latte")
	sd.show(-1, "Ada", "latte")
	time.Sleep(50 * time.Millisecond)
	if n := len(lcd.commands()); n != 2 {
		t.Fatalf("slot 0 got %d commands, want only the 2 startup ones", n)
	}

	var none *slotDisplays
	none.show(0, "Ada", "latte")
	none.close()
}

func TestValidateServingSlotDisplays(t *testing.T) {
	cfg := validBaseConfig()
	cfg.ServingSlotDisplays = []string{"display-part:lcd-1", "display-part:lcd-2"}
	_, opt, err := cfg.Validate("path")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range cfg.ServingSlotDisplays {
		want := genericcomponent.Named(name).String()
		found := false
		for _, d := range opt {
			found = found || d == want
		}
		if !found {
			t.Errorf("optional deps %v missing %q", opt, want)
		}
	}

	cfg.ServingSlotDisplays = []string{"lcd-1", ""}
	if _, _, err := cfg.Validate("path"); err == nil {
		t.Fatal("expected an error for an empty display name")
	}
}
