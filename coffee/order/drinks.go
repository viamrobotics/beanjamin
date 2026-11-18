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
// into the cup before it is tipped over the ice, and an americano is a lungo
// brewed into a mug and then topped up with hot water.
func IsLungo(drink string) bool {
	return drink == "lungo" || drink == "americano" || IsIced(drink)
}

// IsMug reports whether the drink is served in a mug the arm fetches by vision
// (gated by can_serve_mug_drinks) rather than the espresso cup or the iced glass.
func IsMug(drink string) bool {
	return drink == "americano" || drink == "hot_water"
}

// BrewsCoffee reports whether the drink runs the grind-and-brew cycle. Hot
// water is the one drink that doesn't: it is dispensed straight into the mug.
func BrewsCoffee(drink string) bool {
	return drink != "hot_water"
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

// TakesSpice reports whether the drink may be ordered with a dash of spice,
// shaken over the staged glass before it is served (coffee/spice.go).
func TakesSpice(drink string) bool {
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
	if base, b, ok := ParseLegacyDrink(drink); ok {
		return base, b
	}
	if beans == "" {
		if decaf {
			return drink, beansDecaf
		}
		return drink, BeansRegular
	}
	return drink, beans
}

// The drink ids decaf was ordered by before beans were their own field. Older
// clients (the kiosk's decaf toggle, voice-command and Stream Deck configs, the
// Slack ordering bot) still send and expect them, so the service keeps
// speaking them at its edges — accepted on prepare_order (ParseLegacyDrink),
// and reported for decaf espresso and lungo wherever a drink id goes out
// (WireDrink) — until those clients send beans. Remove both together.
const (
	legacyDecaf      = "decaf"
	legacyDecafLungo = "decaf_lungo"
	beansDecaf       = "decaf"
)

// ParseLegacyDrink maps a legacy decaf drink id onto the drink and beans it
// stands for; ok is false for any other id.
func ParseLegacyDrink(drink string) (base, beans string, ok bool) {
	switch drink {
	case legacyDecaf:
		return "espresso", beansDecaf, true
	case legacyDecafLungo:
		return "lungo", beansDecaf, true
	}
	return "", "", false
}

// WireDrink is the drink id to report to clients for drink made from beans:
// the drink itself, except a decaf espresso or lungo, which keeps its legacy
// id ("decaf", "decaf_lungo") so clients that predate beans still recognize
// it and can order it again. Reported alongside beans, which is exact.
func WireDrink(drink, beans string) string {
	if beans != beansDecaf {
		return drink
	}
	switch drink {
	case "espresso":
		return legacyDecaf
	case "lungo":
		return legacyDecafLungo
	}
	return drink
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
	// Milks are the milks in the fridge, by the name an order asks for one with.
	// The first is the default for a milk drink that names none.
	Milks []string
	// MugDrinks gates the drinks served in a mug (the americano and hot water).
	MugDrinks bool
	// Spices are the spices on the rack, by the name an order asks for one with.
	// Empty when the machine offers no spice.
	Spices []string
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
	case "americano", "hot_water":
		return m.MugDrinks, "can_serve_mug_drinks=false"
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

// ResolveSpice validates an order's spice choice against the menu. A spice is
// optional, so "" is always accepted; any other value must be one of the
// menu's spices on a drink that takes one. As with milk, a spice the drink
// can't have is an error rather than silently dropped.
func (m Menu) ResolveSpice(drink, spice string) (string, error) {
	if spice == "" {
		return "", nil
	}
	if !TakesSpice(drink) {
		return "", fmt.Errorf("spice %q given for %q, which takes no spice", spice, drink)
	}
	if len(m.Spices) == 0 {
		return "", fmt.Errorf("spice %q given, but this machine offers no spice", spice)
	}
	if !slices.Contains(m.Spices, spice) {
		return "", fmt.Errorf("unknown spice %q, available: %s", spice, strings.Join(m.Spices, ", "))
	}
	return spice, nil
}
