package postprocess

// Cloud voice transcription via an OpenRouter audio model. A pure
// in-process HTTP client (no subprocess, no cgo) so it works identically
// on macOS, Windows, and Linux. WhatsApp voice notes are Ogg/Opus and are
// sent verbatim — no local transcoding step.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

const (
	// DefaultVoiceModel is a cheap, strong multilingual audio model
	// (validated on Arabic/English WhatsApp voice notes). It must be an
	// audio-input model — unlike DefaultCloudModel, which is image-only.
	DefaultVoiceModel = "google/gemini-2.5-flash"

	// voiceMaxOutputTokens is generous: a long voice note (10+ min) can
	// transcribe to a few thousand tokens, and a truncated transcript is
	// worse than a slightly pricier call.
	voiceMaxOutputTokens = 8192

	// voiceTranscribePrompt asks for a verbatim, translation-free
	// transcript in the original language (chats mix Arabic / English).
	voiceTranscribePrompt = "Transcribe this voice message verbatim. Output ONLY the " +
		"transcription text in its original language (Arabic, English, etc.) with no " +
		"translation, no commentary, and no quotation marks. If there is no speech, output nothing."
)

// voiceTranscriber transcribes one Ogg/Opus clip per call.
type voiceTranscriber struct {
	client *http.Client
	apiKey string
	model  string

	mu      sync.Mutex // guards costUSD (Transcribe runs from a worker pool)
	costUSD float64
}

// newVoiceTranscriber builds a transcriber. Empty model → DefaultVoiceModel;
// empty apiKey is an error.
func newVoiceTranscriber(apiKey, model string) (*voiceTranscriber, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("cloud transcriber: OpenRouter API key required")
	}
	if model == "" {
		model = DefaultVoiceModel
	}
	return &voiceTranscriber{
		client: &http.Client{Timeout: cloudHTTPTimeout},
		apiKey: apiKey,
		model:  model,
	}, nil
}

func (t *voiceTranscriber) Model() string { return t.model }

// CostUSD returns the running total of per-request cost OpenRouter has
// reported so far.
func (t *voiceTranscriber) CostUSD() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.costUSD
}

func (t *voiceTranscriber) addCost(usd float64) {
	t.mu.Lock()
	t.costUSD += usd
	t.mu.Unlock()
}

// Transcribe sends the raw Ogg/Opus bytes and returns the transcript. A
// per-clip failure is a normal error; a global auth/billing failure is a
// *FatalError (from openRouterCall) that aborts the whole run.
func (t *voiceTranscriber) Transcribe(ctx context.Context, opus []byte) (string, error) {
	b64 := base64.StdEncoding.EncodeToString(opus)
	body, err := json.Marshal(chatRequest{
		Model: t.model,
		Messages: []chatMessage{{
			Role: "user",
			Content: []contentPart{
				{Type: "text", Text: voiceTranscribePrompt},
				{Type: "input_audio", InputAudio: &inputAudioPart{Data: b64, Format: "ogg"}},
			},
		}},
		MaxTokens:   voiceMaxOutputTokens,
		Temperature: 0,
		Usage:       &usageRequest{Include: true},
	})
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}
	cr, err := openRouterCall(ctx, t.client, t.apiKey, body)
	if err != nil {
		return "", err
	}
	if cr.Usage != nil {
		t.addCost(cr.Usage.Cost)
	}
	return strings.TrimSpace(cr.Choices[0].Message.Content), nil
}
