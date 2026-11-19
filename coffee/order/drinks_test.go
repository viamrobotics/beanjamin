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

func TestLegacyDrinkIDs(t *testing.T) {
	for _, tt := range []struct{ id, base, beans string }{
		{"decaf", "espresso", "decaf"},
		{"decaf_lungo", "lungo", "decaf"},
	} {
		base, beans, ok := ParseLegacyDrink(tt.id)
		if !ok || base != tt.base || beans != tt.beans {
			t.Errorf("ParseLegacyDrink(%q) = %q, %q, %v", tt.id, base, beans, ok)
		}
		// And the round trip: what goes out is what old clients send back.
		if got := WireDrink(tt.base, tt.beans); got != tt.id {
			t.Errorf("WireDrink(%q, %q) = %q, want %q", tt.base, tt.beans, got, tt.id)
		}
	}
	if _, _, ok := ParseLegacyDrink("espresso"); ok {
		t.Error("espresso is not a legacy id")
	}
	// Anything old clients had no id for goes out as the plain drink.
	for _, tt := range []struct{ drink, beans, want string }{
		{"espresso", "regular", "espresso"},
		{"espresso", "light_roast", "espresso"},
		{"iced_latte", "decaf", "iced_latte"},
	} {
		if got := WireDrink(tt.drink, tt.beans); got != tt.want {
			t.Errorf("WireDrink(%q, %q) = %q, want %q", tt.drink, tt.beans, got, tt.want)
		}
	}
}

func TestMenuResolveSpice(t *testing.T) {
	menu := Menu{Iced: true, IcedLatte: true, Milks: []string{"whole"}, Spices: []string{"cinnamon", "nutmeg"}}
	for _, tt := range []struct {
		drink, spice, want string
		wantErr            bool
	}{
		{drink: "iced_latte", spice: "", want: ""}, // no spice is the default
		{drink: "iced_latte", spice: "nutmeg", want: "nutmeg"},
		{drink: "iced_latte", spice: "paprika", wantErr: true},
		{drink: "espresso", spice: "", want: ""},
		{drink: "iced_coffee", spice: "cinnamon", wantErr: true}, // only the latte takes spice
	} {
		got, err := menu.ResolveSpice(tt.drink, tt.spice)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("ResolveSpice(%q, %q) = %q, %v; want %q, err=%v", tt.drink, tt.spice, got, err, tt.want, tt.wantErr)
		}
	}
	if _, err := (Menu{IcedLatte: true}).ResolveSpice("iced_latte", "cinnamon"); err == nil {
		t.Error("ResolveSpice with no spices configured should fail")
	}
}

func TestMenuResolveMilk(t *testing.T) {
	menu := Menu{Iced: true, IcedLatte: true, Milks: []string{"whole", "oat"}}
	tests := []struct {
		drink, milk, want string
		wantErr           bool
	}{
		{drink: "iced_latte", milk: "", want: "whole"}, // the first milk is the default
		{drink: "iced_latte", milk: "oat", want: "oat"},
		{drink: "iced_latte", milk: "soy", wantErr: true},
		{drink: "espresso", milk: "", want: ""},
		{drink: "espresso", milk: "oat", wantErr: true}, // milk on a drink without milk
	}
	for _, tt := range tests {
		got, err := menu.ResolveMilk(tt.drink, tt.milk)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ResolveMilk(%q, %q) = %q, %v; want %q, err=%v", tt.drink, tt.milk, got, err, tt.want, tt.wantErr)
		}
	}
	if _, err := (Menu{IcedLatte: true}).ResolveMilk("iced_latte", ""); err == nil {
		t.Error("ResolveMilk with no milks configured should fail")
	}
}

func TestMugDrinkPredicates(t *testing.T) {
	tests := []struct {
		drink                   string
		mug, brewsCoffee, lungo bool
	}{
		{"americano", true, true, true},
		{"hot_water", true, false, false},
		{"lungo", false, true, true},
		{"espresso", false, true, false},
		{"iced_coffee", false, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.drink, func(t *testing.T) {
			if got := IsMug(tt.drink); got != tt.mug {
				t.Errorf("IsMug = %v, want %v", got, tt.mug)
			}
			if got := BrewsCoffee(tt.drink); got != tt.brewsCoffee {
				t.Errorf("BrewsCoffee = %v, want %v", got, tt.brewsCoffee)
			}
			if got := IsLungo(tt.drink); got != tt.lungo {
				t.Errorf("IsLungo = %v, want %v", got, tt.lungo)
			}
		})
	}
	if got := DrinkLabel("hot_water", ""); got != "hot water" {
		t.Errorf("DrinkLabel(hot_water) = %q, want %q", got, "hot water")
	}
}

func TestSupportsMugDrinks(t *testing.T) {
	for _, drink := range []string{"americano", "hot_water"} {
		if ok, reason := (Menu{}).Supports(drink); ok || reason != "can_serve_mug_drinks=false" {
			t.Errorf("Supports(%q) off = %v, %q", drink, ok, reason)
		}
		if ok, _ := (Menu{MugDrinks: true}).Supports(drink); !ok {
			t.Errorf("Supports(%q) on = false, want true", drink)
		}
	}
}
