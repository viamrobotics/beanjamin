package coffee

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
var orderReceivedAnonymous = []string{
	"Order received! One %[1]s in the queue.",
	"Got it! %[1]s coming up.",
	"Thanks! Your %[1]s is in line.",
	"%[1]s — added to the queue.",
	"Nice! One %[1]s, logged and queued.",
}

var orderReceivedNamed = []string{
	"Order received, %[2]s — %[1]s in the queue.",
	"Got it, %[2]s! %[1]s coming up.",
	"Thanks %[2]s, your %[1]s is in line.",
	"%[2]s, your %[1]s is on the list.",
	"Nice one %[2]s — one %[1]s, logged and queued.",
}

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

// drinkReadyBatch templates are spoken at cup handoff for orders that are
// part of a multi-drink batch. Format verbs: %[1]s drink, %[2]s name,
// %[3]d batch index (1-based), %[4]d batch size.
var drinkReadyBatchAnonymous = []string{
	"Your %[1]s is ready — number %[3]d of %[4]d.",
	"Here you go, fresh %[1]s. That's %[3]d of %[4]d.",
	"%[1]s up — %[3]d out of %[4]d.",
}

var drinkReadyBatchNamed = []string{
	"%[2]s, your %[1]s is ready — number %[3]d of %[4]d.",
	"Here you go %[2]s, fresh %[1]s. That's %[3]d of %[4]d.",
	"%[1]s number %[3]d for %[2]s — %[4]d total.",
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

func pickOrderReceived(drink, customerName string) string {
	drink = speakableDrink(drink)
	if customerName != "" {
		return fmt.Sprintf(orderReceivedNamed[rand.Intn(len(orderReceivedNamed))], drink, customerName)
	}
	return fmt.Sprintf(orderReceivedAnonymous[rand.Intn(len(orderReceivedAnonymous))], drink)
}

// orderReceivedBatch templates fire once per batch (count > 1), replacing
// the per-order pickOrderReceived line that would otherwise speak N-1 times
// in rapid succession at enqueue time. Format verbs: %[1]d count,
// %[2]s drink-plural, %[3]s name.
var orderReceivedBatchAnonymous = []string{
	"%[1]d %[2]s queued — first one's coming right up.",
	"Got it — %[1]d %[2]s in line.",
	"%[1]d %[2]s, on the way.",
}

var orderReceivedBatchNamed = []string{
	"%[1]d %[2]s for %[3]s — first one's coming up.",
	"On it %[3]s — %[1]d %[2]s queued.",
	"%[3]s, %[1]d %[2]s in the queue. Starting on the first now.",
}

func pickOrderReceivedBatch(drink, customerName string, count int) string {
	plural := speakableDrink(drink) + "s"
	if customerName != "" {
		return fmt.Sprintf(orderReceivedBatchNamed[rand.Intn(len(orderReceivedBatchNamed))], count, plural, customerName)
	}
	return fmt.Sprintf(orderReceivedBatchAnonymous[rand.Intn(len(orderReceivedBatchAnonymous))], count, plural)
}

func pickAlmostReady() string {
	return almostReadyAnonymous[rand.Intn(len(almostReadyAnonymous))]
}

// pickDrinkReady picks a cup-handoff line. When batchSize > 1, the order is
// part of a multi-drink batch and the spoken line names the position
// (e.g. "2 of 3") so the customer can track progress; otherwise the original
// single-drink templates are used.
func pickDrinkReady(drink, customerName string, batchIndex, batchSize int) string {
	drink = speakableDrink(drink)
	if batchSize > 1 {
		if customerName != "" {
			return fmt.Sprintf(drinkReadyBatchNamed[rand.Intn(len(drinkReadyBatchNamed))], drink, customerName, batchIndex, batchSize)
		}
		return fmt.Sprintf(drinkReadyBatchAnonymous[rand.Intn(len(drinkReadyBatchAnonymous))], drink, customerName, batchIndex, batchSize)
	}
	if customerName != "" {
		return fmt.Sprintf(drinkReadyNamed[rand.Intn(len(drinkReadyNamed))], drink, customerName)
	}
	return fmt.Sprintf(drinkReadyAnonymous[rand.Intn(len(drinkReadyAnonymous))], drink)
}

func pickUnsupportedDrink(drink string) string {
	return fmt.Sprintf(unsupportedDrink[rand.Intn(len(unsupportedDrink))], speakableDrink(drink))
}

// orderFailed lines are Cappuccina owning up to a brew that genuinely faulted.
// Spoken via sayAlways alongside the red LED flash (fault_alert.go); operator
// cancels get the calmer cancelAnnouncement instead. Format verbs:
// %[1]s = drink name, %[2]s = customer name.
var orderFailedAnonymous = []string{
	"Ugh. That %[1]s did not make it. I'd blame the beans, but we all saw whose arm it was.",
	"Well. The %[1]s is officially a crime scene. A human will be along shortly.",
	"Bad news: no %[1]s. Good news: I remain delightful. Someone's coming to check on me.",
	"That %[1]s fought back and won. Rematch as soon as a human sorts me out.",
	"I have one arm and today it chose chaos. Your %[1]s is a casualty — help is on the way.",
}

var orderFailedNamed = []string{
	"%[2]s, look away. Your %[1]s did not survive. A human will be along to avenge it.",
	"%[2]s, I fumbled your %[1]s. In my defense, I've been awake since I was plugged in.",
	"Sorry %[2]s — the %[1]s is a no-go. I blame the humidity. And, frankly, the humans.",
	"%[2]s, your %[1]s and I had creative differences. A human is coming to mediate.",
	"Not my finest work, %[2]s. The %[1]s is gone. Tell no one — someone's on the way to fix me.",
}

func pickOrderFailed(drink, customerName string) string {
	drink = speakableDrink(drink)
	if customerName != "" {
		return fmt.Sprintf(orderFailedNamed[rand.Intn(len(orderFailedNamed))], drink, customerName)
	}
	return fmt.Sprintf(orderFailedAnonymous[rand.Intn(len(orderFailedAnonymous))], drink)
}
