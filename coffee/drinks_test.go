package coffee

import (
	"testing"
	"time"
)

// TestDrinkClassifiers locks down the drink catalog: which names map to the
// decaf grind path, the lungo pour size, the iced serving path, the fridge milk
// step, and the water-usage increment.
func TestDrinkClassifiers(t *testing.T) {
	tests := []struct {
		drink                    string
		decaf, lungo, iced, milk bool
		water                    float64
	}{
		{"espresso", false, false, false, false, 1},
		{"lungo", false, true, false, false, 1.5},
		{"decaf", true, false, false, false, 1},
		{"decaf_lungo", true, true, false, false, 1.5},
		{"iced_coffee", false, false, true, false, 1},
		// An iced latte is an iced drink that additionally gets milk, so it must
		// take the iced serving path as well as the milk one.
		{"iced_latte", false, false, true, true, 1},
		{"unknown", false, false, false, false, 1},
	}
	for _, tt := range tests {
		t.Run(tt.drink, func(t *testing.T) {
			if got := isDecafDrink(tt.drink); got != tt.decaf {
				t.Errorf("isDecafDrink = %v, want %v", got, tt.decaf)
			}
			if got := isLungoDrink(tt.drink); got != tt.lungo {
				t.Errorf("isLungoDrink = %v, want %v", got, tt.lungo)
			}
			if got := isIcedDrink(tt.drink); got != tt.iced {
				t.Errorf("isIcedDrink = %v, want %v", got, tt.iced)
			}
			if got := isMilkDrink(tt.drink); got != tt.milk {
				t.Errorf("isMilkDrink = %v, want %v", got, tt.milk)
			}
			if got := waterDelta(tt.drink); got != tt.water {
				t.Errorf("waterDelta = %v, want %v", got, tt.water)
			}
		})
	}
}

// TestDrinkBrewTime covers the lungo-vs-espresso branch and the configured
// seconds -> Duration conversion.
func TestDrinkBrewTime(t *testing.T) {
	def := &beanjaminCoffee{cfg: &Config{}}
	defaults := map[string]time.Duration{
		"espresso":    defaultEspressoBrewTime,
		"decaf":       defaultEspressoBrewTime,
		"lungo":       defaultLungoBrewTime,
		"decaf_lungo": defaultLungoBrewTime,
	}
	for drink, want := range defaults {
		if got := def.drinkBrewTime(drink); got != want {
			t.Errorf("default drinkBrewTime(%q) = %v, want %v", drink, got, want)
		}
	}

	set := &beanjaminCoffee{cfg: &Config{BrewTimeSec: 10, LungoBrewTimeSec: 20}}
	if got := set.drinkBrewTime("espresso"); got != 10*time.Second {
		t.Errorf("configured espresso brew = %v, want 10s", got)
	}
	if got := set.drinkBrewTime("decaf_lungo"); got != 20*time.Second {
		t.Errorf("configured decaf_lungo brew = %v, want 20s", got)
	}
}

// TestGrindAndIceDurations covers the configured-or-default second durations.
func TestGrindAndIceDurations(t *testing.T) {
	def := &beanjaminCoffee{cfg: &Config{}}
	if got := def.grindDurationSec(); got != defaultGrindTimeSec {
		t.Errorf("default grindDurationSec = %v, want %v", got, defaultGrindTimeSec)
	}
	if got := def.iceDispenseSec(); got != defaultIceDispenseSec {
		t.Errorf("default iceDispenseSec = %v, want %v", got, defaultIceDispenseSec)
	}
	if got := def.milkPourDwell(); got != time.Duration(defaultMilkPourSec*float64(time.Second)) {
		t.Errorf("default milkPourDwell = %v, want %vs", got, defaultMilkPourSec)
	}
	if got := def.buttonPressHold(); got != 500*time.Millisecond {
		t.Errorf("default buttonPressHold = %v, want 500ms", got)
	}

	if got := def.pourMoveOptions().MaxVelDegsPerSec; got != defaultPourVelDegsPerSec {
		t.Errorf("default pour velocity = %v, want %v", got, defaultPourVelDegsPerSec)
	}
	if got := def.pourMoveOptions().MaxAccDegsPerSec2; got != 0 {
		t.Errorf("default pour acceleration = %v, want 0 (arm default)", got)
	}

	set := &beanjaminCoffee{cfg: &Config{GrindTimeSec: 3, IceDispenseSec: 9, MilkPourSec: 6.5, PourVelDegsPerSec: 42, PourAccDegsPerSec2: 300, ButtonPressHoldSec: 1.25}}
	if got := set.grindDurationSec(); got != 3 {
		t.Errorf("configured grindDurationSec = %v, want 3", got)
	}
	if got := set.buttonPressHold(); got != 1250*time.Millisecond {
		t.Errorf("configured buttonPressHold = %v, want 1.25s", got)
	}
	if got := set.iceDispenseSec(); got != 9 {
		t.Errorf("configured iceDispenseSec = %v, want 9", got)
	}
	if got := set.milkPourDwell(); got != 6500*time.Millisecond {
		t.Errorf("configured milkPourDwell = %v, want 6.5s", got)
	}
	if got := set.pourMoveOptions().MaxVelDegsPerSec; got != 42 {
		t.Errorf("configured pour velocity = %v, want 42", got)
	}
	if got := set.pourMoveOptions().MaxAccDegsPerSec2; got != 300 {
		t.Errorf("configured pour acceleration = %v, want 300", got)
	}
}
