package speech

import (
	"context"
	"testing"

	"go.viam.com/rdk/testutils/inject"
)

func recordingSpeech(said *[]string) *inject.GenericService {
	res := inject.NewGenericService("speech")
	res.DoFunc = func(_ context.Context, cmd map[string]any) (map[string]any, error) {
		if text, ok := cmd["say_async"].(string); ok {
			*said = append(*said, text)
		}
		return map[string]any{}, nil
	}
	return res
}

func TestSpeaker_SayRespectsConversational(t *testing.T) {
	ctx := context.Background()
	for _, conversational := range []bool{false, true} {
		var said []string
		s := NewSpeaker(recordingSpeech(&said), conversational)
		if err := s.Say(ctx, "narration"); err != nil {
			t.Fatalf("Say: %v", err)
		}
		if err := s.SayAlways(ctx, "handoff"); err != nil {
			t.Fatalf("SayAlways: %v", err)
		}
		want := []string{"handoff"}
		if conversational {
			want = []string{"narration", "handoff"}
		}
		if len(said) != len(want) || said[len(said)-1] != "handoff" || said[0] != want[0] {
			t.Errorf("conversational=%v: said %v, want %v", conversational, said, want)
		}
	}
}

func TestSpeaker_UnconfiguredIsSilent(t *testing.T) {
	ctx := context.Background()
	for name, s := range map[string]*Speaker{"nil": nil, "no_resource": NewSpeaker(nil, true)} {
		if s.Configured() {
			t.Errorf("%s: Configured() = true", name)
		}
		if err := s.Say(ctx, "x"); err != nil {
			t.Errorf("%s: Say: %v", name, err)
		}
		if err := s.SayAlways(ctx, "x"); err != nil {
			t.Errorf("%s: SayAlways: %v", name, err)
		}
	}
}
