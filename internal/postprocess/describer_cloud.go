package postprocess

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// cloudDescriber describes images via an OpenRouter vision model. It
// is a pure in-process HTTP client — no subprocess, no cgo — so it
// works identically on macOS, Windows, and Linux (the only describer
// on non-macOS, where Apple Vision is unavailable).
//
// One model call per image returns a structured reply that we split
// into verbatim OCR (wa_image_text.ocr_text) and a short summary
// (wa_image_text.description); see cloudDescribePrompt and
// splitTextDescription.

const (
	openRouterURL = "https://openrouter.ai/api/v1/chat/completions"

	// DefaultCloudModel is a cheap, strong multilingual-OCR default.
	DefaultCloudModel = "qwen/qwen3-vl-8b-instruct"

	cloudMaxOutputTokens = 700
	cloudHTTPTimeout     = 120 * time.Second
	cloudMaxRetries      = 5

	// cloudDescribePrompt forces a required, separated transcription
	// so the model can't quietly summarize away the OCR (the literal
	// numbers/names/dates are the searchable value of an archive).
	cloudDescribePrompt = "You are indexing a personal chat archive. Output EXACTLY two labeled " +
		"sections and nothing else:\n\n" +
		"TEXT: Transcribe every piece of visible text VERBATIM, in its original " +
		"language and script (Arabic, English, digits, @handles, prices, dates). " +
		"Preserve reading/line order. Do NOT translate or summarize. If the image " +
		"has no readable text, write 'none'.\n\n" +
		"DESCRIPTION: 1-2 factual sentences describing the scene, people, and objects."
)

type cloudDescriber struct {
	client *http.Client
	apiKey string
	model  string

	mu      sync.Mutex // guards costUSD (Describe runs from a worker pool)
	costUSD float64    // running total of OpenRouter-reported request cost
}

// newCloudDescriber builds a cloud describer. An empty model falls
// back to DefaultCloudModel; an empty apiKey is an error.
func newCloudDescriber(apiKey, model string) (*cloudDescriber, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("cloud describer: OpenRouter API key required")
	}
	if model == "" {
		model = DefaultCloudModel
	}
	return &cloudDescriber{
		client: &http.Client{Timeout: cloudHTTPTimeout},
		apiKey: apiKey,
		model:  model,
	}, nil
}

// ValidateOpenRouterKey does a cheap, token-free auth check (GET
// /api/v1/key) so the UI can confirm a pasted key works before a run.
// A nil return means the key is accepted.
func ValidateOpenRouterKey(ctx context.Context, apiKey string) error {
	if strings.TrimSpace(apiKey) == "" {
		return errors.New("empty API key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://openrouter.ai/api/v1/key", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	return fmt.Errorf("OpenRouter rejected the key (HTTP %d): %s",
		resp.StatusCode, strings.TrimSpace(string(body)))
}

func (c *cloudDescriber) Source() string { return SourceCloud }
func (c *cloudDescriber) Model() string  { return c.model }
func (c *cloudDescriber) Close() error   { return nil }

// CostUSD returns the running total of per-request cost OpenRouter has
// reported so far. Satisfies the costReporter interface MediaIndex
// uses to surface a live spend figure in progress events.
func (c *cloudDescriber) CostUSD() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.costUSD
}

func (c *cloudDescriber) addCost(usd float64) {
	c.mu.Lock()
	c.costUSD += usd
	c.mu.Unlock()
}

func (c *cloudDescriber) Describe(ctx context.Context, _ int64, _ string, data []byte) (Description, error) {
	ext, ok := detectImageFormat(data)
	if !ok {
		return Description{}, errors.New("unrecognized image format")
	}
	dataURI := "data:" + mimeForImageExt(ext) + ";base64," + base64.StdEncoding.EncodeToString(data)

	body, err := json.Marshal(chatRequest{
		Model: c.model,
		Messages: []chatMessage{{
			Role: "user",
			Content: []contentPart{
				{Type: "text", Text: cloudDescribePrompt},
				{Type: "image_url", ImageURL: &imageURLPart{URL: dataURI}},
			},
		}},
		MaxTokens:   cloudMaxOutputTokens,
		Temperature: 0.1,
		Usage:       &usageRequest{Include: true},
	})
	if err != nil {
		return Description{}, fmt.Errorf("encode request: %w", err)
	}

	cr, err := c.call(ctx, body)
	if err != nil {
		return Description{}, err
	}
	if cr.Usage != nil {
		c.addCost(cr.Usage.Cost)
	}
	ocr, desc := splitTextDescription(cr.Choices[0].Message.Content)
	return Description{OCRText: ocr, Description: desc}, nil
}

// call POSTs one chat-completion request via the shared OpenRouter
// client (retry/backoff/FatalError handling lives in openRouterCall).
func (c *cloudDescriber) call(ctx context.Context, body []byte) (*chatResponse, error) {
	return openRouterCall(ctx, c.client, c.apiKey, body)
}

// openRouterCall POSTs one chat-completion request, retrying on 429/5xx
// with exponential backoff that honours ctx cancellation. Auth/billing
// failures (401/402/403) return a FatalError so a run aborts instead of
// marking every row errored. Shared by the image describer and the voice
// transcriber. Returns the decoded (validated, non-empty-choices) response.
func openRouterCall(ctx context.Context, client *http.Client, apiKey string, body []byte) (*chatResponse, error) {
	var lastErr error
	for attempt := 0; attempt < cloudMaxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		// OpenRouter attribution headers (optional, recommended).
		req.Header.Set("HTTP-Referer", "https://github.com/whatskept")
		req.Header.Set("X-Title", "whatskept")

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			if !backoff(ctx, attempt) {
				return nil, ctx.Err()
			}
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("openrouter http %d: %s", resp.StatusCode, clip(respBody, 200))
			if !backoff(ctx, attempt) {
				return nil, ctx.Err()
			}
			continue
		case resp.StatusCode == http.StatusUnauthorized ||
			resp.StatusCode == http.StatusPaymentRequired ||
			resp.StatusCode == http.StatusForbidden:
			// Auth / billing failures are GLOBAL, not per-image: a bad
			// key or an empty balance fails every request. Surface a
			// FatalError so the run aborts immediately instead of
			// marking thousands of rows as individually "errored".
			return nil, &FatalError{Msg: fmt.Sprintf(
				"OpenRouter rejected the request (HTTP %d): %s — check your API key and credit balance",
				resp.StatusCode, clip(respBody, 160))}
		case resp.StatusCode != http.StatusOK:
			return nil, fmt.Errorf("openrouter http %d: %s", resp.StatusCode, clip(respBody, 200))
		}

		var cr chatResponse
		if err := json.Unmarshal(respBody, &cr); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
		if cr.Error != nil && cr.Error.Message != "" {
			return nil, fmt.Errorf("openrouter: %s", cr.Error.Message)
		}
		if len(cr.Choices) == 0 {
			return nil, errors.New("openrouter: response had no choices")
		}
		return &cr, nil
	}
	return nil, fmt.Errorf("openrouter: exhausted retries: %w", lastErr)
}

// backoff sleeps 2^attempt seconds, returning false if ctx is
// cancelled during the wait (so the caller can abort cleanly).
func backoff(ctx context.Context, attempt int) bool {
	t := time.NewTimer(time.Duration(int64(1)<<uint(attempt)) * time.Second)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// splitTextDescription parses the model's "TEXT: … DESCRIPTION: …"
// reply into (ocr, description). If the markers are absent it treats
// the whole reply as the description (no OCR). A literal "none" OCR
// body normalises to "".
func splitTextDescription(s string) (ocr, desc string) {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	di := strings.Index(lower, "description:")
	if di < 0 {
		return "", s
	}
	head := strings.TrimSpace(s[:di])
	desc = strings.TrimSpace(s[di+len("description:"):])
	if strings.HasPrefix(strings.ToLower(head), "text:") {
		head = strings.TrimSpace(head[len("text:"):])
	}
	if strings.EqualFold(head, "none") {
		head = ""
	}
	return head, desc
}

// mimeForImageExt maps detectImageFormat's bare extension to a MIME
// type for the data URI. Defaults to JPEG (the overwhelming majority
// of WhatsApp media).
func mimeForImageExt(ext string) string {
	switch ext {
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "heic":
		return "image/heic"
	default:
		return "image/jpeg"
	}
}

func clip(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}

// --- OpenRouter wire types (OpenAI-compatible chat completions) -----

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
	Usage       *usageRequest `json:"usage,omitempty"`
}

type usageRequest struct {
	Include bool `json:"include"`
}

type chatMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ImageURL   *imageURLPart   `json:"image_url,omitempty"`
	InputAudio *inputAudioPart `json:"input_audio,omitempty"`
}

type imageURLPart struct {
	URL string `json:"url"`
}

// inputAudioPart carries base64 audio for transcription models. `Format`
// is the container hint (e.g. "ogg" for WhatsApp's Ogg/Opus voice notes).
type inputAudioPart struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		Cost float64 `json:"cost"` // USD; present because we set usage.include
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}
