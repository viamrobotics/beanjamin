package beanjamin

import (
	"fmt"
	"math/rand"
	"strings"
)

// speakableDrink converts a drink id like "decaf_lungo" into a form Google TTS
// reads naturally ("decaf lungo") rather than pronouncing the underscore.
func speakableDrink(drink string) string {
	return strings.ReplaceAll(drink, "_", " ")
}

// Greeting templates use indexed format verbs: %[1]s = drink name, %[2]s = customer name.
var greetingsAnonymous = []string{
	"One %[1]s coming right up!",
	"Great choice! Let me prepare your %[1]s.",
	"It's %[1]s time! Let's get brewing.",
	"Coming right up! One freshly made %[1]s.",
	"Let me whip up your %[1]s!",
}

var greetingsNamed = []string{
	"Hey %[2]s! One %[1]s coming right up!",
	"Great choice, %[2]s! Let me prepare your %[1]s.",
	"%[2]s, it's %[1]s time! Let's get brewing.",
	"Coming right up, %[2]s! One freshly made %[1]s.",
	"Let me whip up your %[1]s, %[2]s!",
}

var almostReadyAnonymous = []string{
	"Your coffee is almost ready!",
	"Almost there! Just a moment.",
	"Hang tight, it's nearly done!",
	"Just putting the finishing touches on your drink.",
	"Your coffee is coming together!",
}

var drinkReadyAnonymous = []string{
	"Your %[1]s is ready!",
	"Here you go, one fresh %[1]s!",
	"Your %[1]s is served!",
}

var drinkReadyNamed = []string{
	"%[2]s, your %[1]s is ready!",
	"Here you go %[2]s, one fresh %[1]s!",
	"%[1]s for %[2]s is served!",
}

var unsupportedDrink = []string{
	// polite
	"I'm sorry, I cannot make a %s at the moment. May I offer you an espresso or a lungo instead?",
	"Unfortunately, %s isn't on the menu yet. How about a nice espresso or lungo?",
	// cheeky
	"A %s? Bold request. I do espresso and lungo, and I do them well.",
	"Look, I'm a focused machine. Espresso or lungo. %s is not in my repertoire.",
	// sassy
	"Oh, you wanted a %s? That's cute. Espresso or lungo. Pick one.",
	"%s? Do I look like a vending machine? Espresso or lungo. That's the deal.",
	// unhinged
	"A %s?! In THIS economy?! You're getting an espresso or a lungo and you'll like it.",
	"Did you just ask me for a %s? I have one arm and zero patience. Espresso, lungo, or nothing.",
}

func pickGreeting(drink, customerName string) string {
	drink = speakableDrink(drink)
	if customerName != "" {
		return fmt.Sprintf(greetingsNamed[rand.Intn(len(greetingsNamed))], drink, customerName)
	}
	return fmt.Sprintf(greetingsAnonymous[rand.Intn(len(greetingsAnonymous))], drink)
}

func pickAlmostReady() string {
	return almostReadyAnonymous[rand.Intn(len(almostReadyAnonymous))]
}

func pickDrinkReady(drink, customerName string) string {
	drink = speakableDrink(drink)
	if customerName != "" {
		return fmt.Sprintf(drinkReadyNamed[rand.Intn(len(drinkReadyNamed))], drink, customerName)
	}
	return fmt.Sprintf(drinkReadyAnonymous[rand.Intn(len(drinkReadyAnonymous))], drink)
}

func pickUnsupportedDrink(drink string) string {
	return fmt.Sprintf(unsupportedDrink[rand.Intn(len(unsupportedDrink))], speakableDrink(drink))
}
