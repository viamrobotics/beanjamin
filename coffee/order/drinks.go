package order

import (
	"fmt"
	"slices"
	"strings"
)

// IsDecaf reports whether the drink uses the decaf grinding path.
func IsDecaf(drink string) bool {
	return drink == "decaf" || drink == "decaf_lungo"
}

// IsLungo reports whether the drink is a lungo-size pour, matching the lungo
// cases in the coffee service's drinkBrewTime. The iced drinks pour a lungo
// into the cup before it is tipped over the ice.
func IsLungo(drink string) bool {
	return drink == "lungo" || drink == "decaf_lungo" || IsIced(drink)
}

// IsIced reports whether the drink uses the iced serving path
// (fetch glass -> dispense ice -> pour espresso over ice) instead of handing
// the espresso cup to the customer. It brews a lungo (see IsLungo).
func IsIced(drink string) bool {
	return drink == "iced_coffee" || drink == "iced_latte"
}

// IsMilk reports whether the iced serving path additionally fetches the
// milk bottle from the fridge and pours it into the glass (coffee/milk.go).
// Every milk drink is also an iced drink — the milk goes into the same staged
// glass, over the ice, before the espresso is poured on top.
func IsMilk(drink string) bool {
	return drink == "iced_latte"
}

// Menu is the set of optional drinks a machine is configured to serve. Plain
// espresso and lungo are always on it.
type Menu struct {
	Decaf bool
	Iced  bool
	// IcedLatte implies Iced (the coffee config's Validate rejects it
	// otherwise), so the one flag is the whole gate for an iced latte.
	IcedLatte bool
	// Milks are the milks in the fridge, by the name an order asks for one with.
	// The first is the default for a milk drink that names none.
	Milks []string
}

// Supports reports whether a machine serving this menu can make drink and,
// when it can't, why: the config flag that disables it, or that the drink is
// unknown.
func (m Menu) Supports(drink string) (supported bool, reason string) {
	switch drink {
	case "espresso", "lungo":
		return true, ""
	case "decaf", "decaf_lungo":
		return m.Decaf, "can_serve_decaf=false"
	case "iced_coffee":
		return m.Iced, "can_serve_iced=false"
	case "iced_latte":
		return m.IcedLatte, "can_serve_iced_latte=false"
	default:
		return false, "unsupported drink"
	}
}

// ResolveMilk validates an order's milk choice against the menu and returns the
// milk the drink will be made with: the requested one, the default (first)
// milk when a milk drink names none, or "" for a drink without milk. Naming a
// milk on a drink that has none is an error rather than silently ignored, so a
// client that thinks it ordered oat milk finds out it didn't.
func (m Menu) ResolveMilk(drink, milk string) (string, error) {
	if !IsMilk(drink) {
		if milk != "" {
			return "", fmt.Errorf("milk %q given for %q, which has no milk", milk, drink)
		}
		return "", nil
	}
	if len(m.Milks) == 0 {
		return "", fmt.Errorf("no milk options are configured")
	}
	if milk == "" {
		return m.Milks[0], nil
	}
	if !slices.Contains(m.Milks, milk) {
		return "", fmt.Errorf("unknown milk %q, available: %s", milk, strings.Join(m.Milks, ", "))
	}
	return milk, nil
}
