package coffee

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.viam.com/rdk/logging"
)

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
