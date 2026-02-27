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

func pickGreeting(customerName string) string {
	if customerName != "" {
		return fmt.Sprintf(greetingsNamed[rand.Intn(len(greetingsNamed))], customerName)
	}
	return greetingsAnonymous[rand.Intn(len(greetingsAnonymous))]
}
