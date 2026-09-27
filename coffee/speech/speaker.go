// Package speech holds what the coffee service says out loud: the phrase
// tables it picks lines from, the Speaker that sends them to the configured
// speech service, and the FaultAlarm that reacts to a genuinely failed order.
package speech

import (
	"context"

	"go.viam.com/rdk/resource"
)

// Speaker sends lines to the speech service. A Speaker without a speech
// resource, or a nil *Speaker, accepts every line and says nothing.
type Speaker struct {
	res            resource.Resource
	conversational bool
}

// NewSpeaker wraps the speech resource, which is nil when speech_service_name
// is not configured. conversational mirrors the coffee service's
// Conversational config and gates Say.
func NewSpeaker(res resource.Resource, conversational bool) *Speaker {
	return &Speaker{res: res, conversational: conversational}
}

// Configured reports whether a speech resource is attached.
func (s *Speaker) Configured() bool {
	return s != nil && s.res != nil
}

// Say queues text for the speech service when conversational mode is
// enabled, otherwise no-ops. Use this for status-narrating lines (greetings,
// progress prompts, rejections) that an external orchestrator may want to
// own instead. For lines that must always be spoken regardless of mode
// (e.g. the drink-ready handoff), use SayAlways.
func (s *Speaker) Say(ctx context.Context, text string) error {
	if s == nil || !s.conversational {
		return nil
	}
	return s.SayAlways(ctx, text)
}

// SayAlways queues text for the speech service via the non-blocking
// say_async DoCommand, regardless of the Conversational config. It
// returns as soon as the text is accepted by the speech service's async
// queue; the audio will be played once any in-flight speech has finished.
// No-op when no speech service is configured.
func (s *Speaker) SayAlways(ctx context.Context, text string) error {
	if !s.Configured() {
		return nil
	}
	_, err := s.res.DoCommand(ctx, map[string]any{
		"say_async": text,
	})
	return err
}
