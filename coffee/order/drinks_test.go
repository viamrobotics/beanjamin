package order

import "testing"

// TestDrinkClassifiers locks down the drink catalog: which names map to the
// decaf grind path, the lungo pour size, the iced serving path, and the fridge
// milk step.
func TestDrinkClassifiers(t *testing.T) {
	tests := []struct {
		drink                    string
		decaf, lungo, iced, milk bool
	}{
		{"espresso", false, false, false, false},
		{"lungo", false, true, false, false},
		{"decaf", true, false, false, false},
		{"decaf_lungo", true, true, false, false},
		{"iced_coffee", false, true, true, false},
		// An iced latte is an iced drink that additionally gets milk, so it must
		// take the iced serving path as well as the milk one.
		{"iced_latte", false, true, true, true},
		{"unknown", false, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.drink, func(t *testing.T) {
			if got := IsDecaf(tt.drink); got != tt.decaf {
				t.Errorf("IsDecaf = %v, want %v", got, tt.decaf)
			}
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
