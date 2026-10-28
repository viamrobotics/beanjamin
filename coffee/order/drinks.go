package order

import (
	"fmt"
	"slices"
	"strings"
)

// BeansRegular is the house beans, ground by the machine's main grinder. Every
// machine serves it, and an order that names no beans gets it.
const BeansRegular = "regular"

// IsLungo reports whether the drink is a lungo-size pour, matching the lungo
// cases in the coffee service's drinkBrewTime. The iced drinks pour a lungo
// into the cup before it is tipped over the ice.
func IsLungo(drink string) bool {
	return drink == "lungo" || IsIced(drink)
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

// DrinkLabel names a drink made from the given beans the way a person would
// say it: "lungo" for the regular beans, "decaf lungo" or "light roast iced
// latte" otherwise. Used for speech and Slack, never as an identifier.
func DrinkLabel(drink, beans string) string {
	label := strings.ReplaceAll(drink, "_", " ")
	if beans == "" || beans == BeansRegular {
		return label
	}
	return strings.ReplaceAll(beans, "_", " ") + " " + label
}

// LegacyDrink maps an order recorded before beans were their own field onto
// the drink and beans it stands for. Back then decaf was a drink id ("decaf",
// "decaf_lungo") and readings carried a decaf flag instead of beans; anything
// already carrying beans is returned unchanged. For readers of stored history
// (order-sensor readings, customer order history), never for new orders.
func LegacyDrink(drink, beans string, decaf bool) (string, string) {
	switch drink {
	case "decaf":
		return "espresso", "decaf"
	case "decaf_lungo":
		return "lungo", "decaf"
	}
	if beans == "" {
		if decaf {
			return drink, "decaf"
		}
		return drink, BeansRegular
	}
	return drink, beans
}

// Menu is the set of drinks and beans a machine is configured to serve. Plain
// espresso and lungo, and the regular beans, are always on it.
type Menu struct {
	// Beans lists the beans the machine can grind besides BeansRegular, one
	// per extra grinder (the coffee config's bean_options).
	Beans []string
	Iced  bool
	// IcedLatte implies Iced (the coffee config's Validate rejects it
	// otherwise), so the one flag is the whole gate for an iced latte.
	IcedLatte bool
}

// Supports reports whether a machine serving this menu can make drink and,
// when it can't, why: the config flag that disables it, or that the drink is
// unknown.
func (m Menu) Supports(drink string) (supported bool, reason string) {
	switch drink {
	case "espresso", "lungo":
		return true, ""
	case "iced_coffee":
		return m.Iced, "can_serve_iced=false"
	case "iced_latte":
		return m.IcedLatte, "can_serve_iced_latte=false"
	default:
		return false, "unsupported drink"
	}
}

// BeanOptions lists every bean this menu offers, BeansRegular first.
func (m Menu) BeanOptions() []string {
	return append([]string{BeansRegular}, m.Beans...)
}

// ResolveBeans validates the beans a prepare_order asked for. Empty means
// BeansRegular; any other value must be one of the menu's beans.
func (m Menu) ResolveBeans(beans string) (string, error) {
	if beans == "" || beans == BeansRegular {
		return BeansRegular, nil
	}
	if slices.Contains(m.Beans, beans) {
		return beans, nil
	}
	return "", fmt.Errorf("unsupported beans %q: this machine offers %s", beans, strings.Join(m.BeanOptions(), ", "))
}
