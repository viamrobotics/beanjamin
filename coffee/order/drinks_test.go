package order

import (
	"slices"
	"testing"
)

// TestDrinkClassifiers locks down the drink catalog: which names map to the
// lungo pour size, the iced serving path, and the fridge milk step.
func TestDrinkClassifiers(t *testing.T) {
	tests := []struct {
		drink             string
		lungo, iced, milk bool
	}{
		{"espresso", false, false, false},
		{"lungo", true, false, false},
		{"iced_coffee", true, true, false},
		// An iced latte is an iced drink that additionally gets milk, so it must
		// take the iced serving path as well as the milk one.
		{"iced_latte", true, true, true},
		{"unknown", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.drink, func(t *testing.T) {
			if got := IsLungo(tt.drink); got != tt.lungo {
				t.Errorf("IsLungo = %v, want %v", got, tt.lungo)
			}
			if got := IsIced(tt.drink); got != tt.iced {
				t.Errorf("IsIced = %v, want %v", got, tt.iced)
			}
			if got := IsMilk(tt.drink); got != tt.milk {
				t.Errorf("IsMilk = %v, want %v", got, tt.milk)
			}
		})
	}
}

// TestMenuSupports checks that decaf is no longer a drink: the old ids are
// rejected like any unknown drink, since decaf is now chosen through beans.
func TestMenuSupports(t *testing.T) {
	m := Menu{Beans: []string{"decaf"}, Iced: true, IcedLatte: true}
	for _, drink := range []string{"espresso", "lungo", "iced_coffee", "iced_latte"} {
		if ok, _ := m.Supports(drink); !ok {
			t.Errorf("Supports(%q) = false, want true", drink)
		}
	}
	for _, drink := range []string{"decaf", "decaf_lungo", "mocha"} {
		if ok, reason := m.Supports(drink); ok || reason != "unsupported drink" {
			t.Errorf("Supports(%q) = %v, %q; want false, unsupported drink", drink, ok, reason)
		}
	}
	if ok, reason := (Menu{}).Supports("iced_coffee"); ok || reason != "can_serve_iced=false" {
		t.Errorf("Supports(iced_coffee) on a hot-only menu = %v, %q", ok, reason)
	}
}

func TestResolveBeans(t *testing.T) {
	m := Menu{Beans: []string{"decaf", "light_roast"}}
	for in, want := range map[string]string{
		"":            BeansRegular,
		"regular":     BeansRegular,
		"decaf":       "decaf",
		"light_roast": "light_roast",
	} {
		got, err := m.ResolveBeans(in)
		if err != nil || got != want {
			t.Errorf("ResolveBeans(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := m.ResolveBeans("dark_roast"); err == nil {
		t.Error("ResolveBeans(dark_roast) should be rejected")
	}
	if _, err := (Menu{}).ResolveBeans("decaf"); err == nil {
		t.Error("decaf should be rejected on a machine with no decaf grinder")
	}
	if got := m.BeanOptions(); !slices.Equal(got, []string{"regular", "decaf", "light_roast"}) {
		t.Errorf("BeanOptions = %v", got)
	}
}

func TestDrinkLabel(t *testing.T) {
	for _, tt := range []struct{ drink, beans, want string }{
		{"espresso", "regular", "espresso"},
		{"espresso", "", "espresso"},
		{"lungo", "decaf", "decaf lungo"},
		{"iced_latte", "light_roast", "light roast iced latte"},
	} {
		if got := DrinkLabel(tt.drink, tt.beans); got != tt.want {
			t.Errorf("DrinkLabel(%q, %q) = %q, want %q", tt.drink, tt.beans, got, tt.want)
		}
	}
}

func TestLegacyDrink(t *testing.T) {
	for _, tt := range []struct {
		drink, beans         string
		decaf                bool
		wantDrink, wantBeans string
	}{
		{"decaf", "", true, "espresso", "decaf"},
		{"decaf_lungo", "", true, "lungo", "decaf"},
		{"espresso", "", false, "espresso", "regular"},
		{"espresso", "", true, "espresso", "decaf"},
		{"iced_latte", "light_roast", false, "iced_latte", "light_roast"},
	} {
		d, b := LegacyDrink(tt.drink, tt.beans, tt.decaf)
		if d != tt.wantDrink || b != tt.wantBeans {
			t.Errorf("LegacyDrink(%q, %q, %v) = %q, %q; want %q, %q",
				tt.drink, tt.beans, tt.decaf, d, b, tt.wantDrink, tt.wantBeans)
		}
	}
}
