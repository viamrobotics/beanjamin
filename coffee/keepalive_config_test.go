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
		// time.LoadLocation("") is UTC, not an error, so an unset timezone has to
		// be rejected explicitly or the window silently means UTC hours.
		{"empty timezone", func(k *KeepAlive) { k.Timezone = "" }},
		{"whitespace timezone", func(k *KeepAlive) { k.Timezone = "  " }},
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
