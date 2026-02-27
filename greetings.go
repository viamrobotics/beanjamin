package beanjamin

import (
	"fmt"
	"math/rand"
)

var greetingsAnonymous = []string{
	"One espresso coming right up!",
	"Great choice! Let me prepare your espresso.",
	"Espresso time! Let's get brewing.",
	"Coming right up! One freshly made espresso.",
	"Let me whip up an espresso for you!",
}

var greetingsNamed = []string{
	"Hey %s! One espresso coming right up!",
	"Great choice, %s! Let me prepare your espresso.",
	"%s, espresso time! Let's get brewing.",
	"Coming right up, %s! One freshly made espresso.",
	"Let me whip up an espresso for you, %s!",
}

var almostReady = []string{
	"%s, your espresso is almost ready!",
	"Almost there, %s! Just a moment.",
	"Hang tight %s, your coffee is nearly done!",
	"%s, just putting the finishing touches on your espresso.",
	"Your espresso is coming together, %s!",
}

var unsupportedDrink = []string{
	// polite
	"I'm sorry, I cannot make a %s at the moment. May I offer you an espresso instead?",
	"Unfortunately, %s isn't on the menu yet. How about a nice espresso?",
	// cheeky
	"A %s? Bold request. I only do espresso, and I do it well.",
	"Look, I'm a one-trick pony and that trick is espresso. %s is not in my repertoire.",
	// sassy
	"Oh, you wanted a %s? That's cute. I make espresso. Period.",
	"%s? Do I look like a vending machine? Espresso. That's the deal.",
	// unhinged
	"A %s?! In THIS economy?! You're getting an espresso and you'll like it.",
	"Did you just ask me for a %s? I have one arm and zero patience. Espresso or nothing.",
}

func pickGreeting(customerName string) string {
	if customerName != "" {
		return fmt.Sprintf(greetingsNamed[rand.Intn(len(greetingsNamed))], customerName)
	}
	return greetingsAnonymous[rand.Intn(len(greetingsAnonymous))]
}

func pickAlmostReady(customerName string) string {
	return fmt.Sprintf(almostReady[rand.Intn(len(almostReady))], customerName)
}

func pickUnsupportedDrink(drink string) string {
	return fmt.Sprintf(unsupportedDrink[rand.Intn(len(unsupportedDrink))], drink)
}
