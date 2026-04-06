// Package texttospeech registers a viam:beanjamin:text-to-speech model that
// implements the rdk:service:generic API. It synthesises audio via the Google
// Cloud Text-to-Speech API and plays it through an rdk:component:audio_out
// dependency.
package texttospeech

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"

	texttospeech "cloud.google.com/go/texttospeech/apiv1"
	texttospeechpb "cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
	"go.viam.com/rdk/components/audioout"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/utils"
	"google.golang.org/api/option"
)

// Google Cloud TTS returns LINEAR16 audio at 24 kHz mono by default.
const defaultSampleRateHz = 24000

var Model = resource.NewModel("viam", "beanjamin", "text-to-speech")

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newTextToSpeech,
		},
	)
}

type Config struct {
	AudioOutName   string                 `json:"audio_out"`
	LanguageCode   string                 `json:"language_code,omitempty"`
	VoiceName      string                 `json:"voice_name,omitempty"`
	GoogleCredJSON map[string]interface{} `json:"google_credentials_json"`
}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.AudioOutName == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "audio_out")
	}
	if len(cfg.GoogleCredJSON) == 0 {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "google_credentials_json")
	}
	return []string{cfg.AudioOutName}, nil, nil
}

type ttsService struct {
	resource.AlwaysRebuild

	name         resource.Name
	logger       logging.Logger
	audioOut     audioout.AudioOut
	ttsClient    *texttospeech.Client
	languageCode string
	voiceName    string
}

func newTextToSpeech(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	ao, err := audioout.FromProvider(deps, conf.AudioOutName)
	if err != nil {
		return nil, fmt.Errorf("audio_out %q not found in dependencies: %w", conf.AudioOutName, err)
	}

	credBytes, err := json.Marshal(conf.GoogleCredJSON)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal google credentials: %w", err)
	}

	ttsClient, err := texttospeech.NewClient(ctx,
		option.WithCredentialsJSON(credBytes),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Google TTS client: %w", err)
	}

	lang := conf.LanguageCode
	if lang == "" {
		lang = "en-US"
	}

	return &ttsService{
		name:         rawConf.ResourceName(),
		logger:       logger,
		audioOut:     ao,
		ttsClient:    ttsClient,
		languageCode: lang,
		voiceName:    conf.VoiceName,
	}, nil
}

func (s *ttsService) Name() resource.Name {
	return s.name
}

func (s *ttsService) Say(ctx context.Context, text string) (string, error) {
	s.logger.Infof("synthesising: %q", text)

	voice := &texttospeechpb.VoiceSelectionParams{
		LanguageCode: s.languageCode,
	}
	if s.voiceName != "" {
		voice.Name = s.voiceName
	}

	resp, err := s.ttsClient.SynthesizeSpeech(ctx, &texttospeechpb.SynthesizeSpeechRequest{
		Input:       &texttospeechpb.SynthesisInput{InputSource: &texttospeechpb.SynthesisInput_Text{Text: text}},
		Voice:       voice,
		AudioConfig: &texttospeechpb.AudioConfig{AudioEncoding: texttospeechpb.AudioEncoding_LINEAR16},
	})
	if err != nil {
		return "", fmt.Errorf("google TTS synthesis failed: %w", err)
	}

	stereo := monoToStereo(resp.AudioContent)
	err = s.audioOut.Play(ctx, stereo, &utils.AudioInfo{
		Codec:        utils.CodecPCM16,
		SampleRateHz: defaultSampleRateHz,
		NumChannels:  2,
	}, nil)
	if err != nil {
		return "", fmt.Errorf("audio_out play failed: %w", err)
	}

	return text, nil
}

func (s *ttsService) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if text, ok := cmd["say"].(string); ok {
		result, err := s.Say(ctx, text)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"text": result}, nil
	}
	return nil, fmt.Errorf("unknown command, supported commands: say")
}

// monoToStereo duplicates each LINEAR16 sample so mono PCM becomes stereo.
func monoToStereo(mono []byte) []byte {
	stereo := make([]byte, len(mono)*2)
	for i := 0; i < len(mono)-1; i += 2 {
		sample := binary.LittleEndian.Uint16(mono[i:])
		binary.LittleEndian.PutUint16(stereo[i*2:], sample)
		binary.LittleEndian.PutUint16(stereo[i*2+2:], sample)
	}
	return stereo
}

func (s *ttsService) Status(ctx context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (s *ttsService) Close(ctx context.Context) error {
	if s.ttsClient != nil {
		return s.ttsClient.Close()
	}
	return nil
}
