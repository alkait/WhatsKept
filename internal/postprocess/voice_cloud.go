package postprocess

// Cloud voice transcription via an OpenRouter audio model. A thin wrapper
// over the shared cloudClient base (cloud.go): the base owns the HTTP
// transport, retries, cost accounting, and model bookkeeping; this file
// only supplies the prompt and the audio content part. WhatsApp voice
// notes are Ogg/Opus and are sent verbatim — no local transcoding step.

import (
	"context"
	"encoding/base64"
	"strings"
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

// voiceTranscriber transcribes one Ogg/Opus clip per call over the shared
// cloud base. Model and CostUSD are promoted from the embedded *cloudClient.
type voiceTranscriber struct {
	*cloudClient
}

// voiceTranscribers is the registry of supported voice-index engines.
// Adding a new audio backend is one entry here (see cloudRegistry). Cloud
// is the only engine today and the default.
var voiceTranscribers = cloudRegistry[*voiceTranscriber]{
	kind:      "voice-index",
	def:       SourceCloud,
	factories: map[string]func(apiKey, model string) (*voiceTranscriber, error){SourceCloud: newVoiceTranscriber},
}

// newVoiceTranscriber builds a transcriber. Empty model → DefaultVoiceModel;
// empty apiKey is an error.
func newVoiceTranscriber(apiKey, model string) (*voiceTranscriber, error) {
	cc, err := newCloudClient(apiKey, model, DefaultVoiceModel)
	if err != nil {
		return nil, err
	}
	return &voiceTranscriber{cc}, nil
}

// Transcribe sends the raw Ogg/Opus bytes and returns the transcript. A
// per-clip failure is a normal error; a global auth/billing failure is a
// *FatalError (from the shared base) that aborts the whole run.
func (t *voiceTranscriber) Transcribe(ctx context.Context, opus []byte) (string, error) {
	b64 := base64.StdEncoding.EncodeToString(opus)
	raw, err := t.complete(ctx, []contentPart{
		{Type: "text", Text: voiceTranscribePrompt},
		{Type: "input_audio", InputAudio: &inputAudioPart{Data: b64, Format: "ogg"}},
	}, voiceMaxOutputTokens, 0)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}
