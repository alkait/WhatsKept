package postprocess

// Shared OpenRouter cloud backend. Every cloud enrichment engine — the
// image describer (describer_cloud.go), the voice transcriber
// (voice_cloud.go), and any future modality — is a thin wrapper over the
// cloudClient base defined here. The base owns the HTTP transport, retry
// and FatalError handling, cost accounting, and model bookkeeping, so a
// new engine only supplies its prompt, its content parts, and how to
// parse the reply. Fix transport/cost/auth behaviour once, here.
//
// cloudClient is a pure in-process HTTP client — no subprocess, no cgo —
// so it works identically on macOS, Windows, and Linux.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	openRouterURL = "https://openrouter.ai/api/v1/chat/completions"

	cloudHTTPTimeout = 120 * time.Second
	cloudMaxRetries  = 5
)

// cloudClient is the shared base for every OpenRouter-backed engine. It
// holds the HTTP client, credentials, selected model, and a running cost
// total. Engines embed it (e.g. cloudDescriber, voiceTranscriber) and call
// complete() to issue one chat-completion turn.
type cloudClient struct {
	client *http.Client
	apiKey string
	model  string

	mu      sync.Mutex // guards costUSD (engines run from a worker pool)
	costUSD float64    // running total of OpenRouter-reported request cost
}

// newCloudClient builds the shared base. An empty model falls back to
// defaultModel; an empty apiKey is an error. Each engine's constructor
// passes its own default model.
func newCloudClient(apiKey, model, defaultModel string) (*cloudClient, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("cloud: OpenRouter API key required")
	}
	if model == "" {
		model = defaultModel
	}
	return &cloudClient{
		client: &http.Client{Timeout: cloudHTTPTimeout},
		apiKey: apiKey,
		model:  model,
	}, nil
}

// Model is the model slug stored alongside results (e.g. wa_image_text.model).
func (c *cloudClient) Model() string { return c.model }

// Close releases any held resources (a no-op for the cloud client; present
// so embedders satisfy interfaces that require it, e.g. Describer).
func (c *cloudClient) Close() error { return nil }

// CostUSD returns the running total of per-request cost OpenRouter has
// reported so far. Satisfies the costReporter interface the pipelines use
// to surface a live spend figure in progress events.
func (c *cloudClient) CostUSD() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.costUSD
}

func (c *cloudClient) addCost(usd float64) {
	c.mu.Lock()
	c.costUSD += usd
	c.mu.Unlock()
}

// complete issues one chat-completion turn: a single user message carrying
// the given content parts. It returns the raw assistant content and accrues
// the OpenRouter-reported cost. A per-request failure is a normal error; a
// global auth/billing failure surfaces as a *FatalError (from openRouterCall)
// so the caller's run aborts instead of marking every row errored.
func (c *cloudClient) complete(ctx context.Context, parts []contentPart, maxTokens int, temperature float64) (string, error) {
	cr, err := c.completeRaw(ctx, parts, nil, maxTokens, temperature)
	if err != nil {
		return "", err
	}
	return cr.Choices[0].Message.Content, nil
}

// completeRaw is complete's lower layer: it returns the decoded response so
// callers that need more than the assistant text (e.g. the document
// extractor, which reads file-parser annotations) can. plugins is optional —
// pass the file-parser plugin to extract PDF text. Cost is accrued here, so
// every cloud call funnels through this one place.
func (c *cloudClient) completeRaw(ctx context.Context, parts []contentPart, plugins []pdfPlugin, maxTokens int, temperature float64) (*chatResponse, error) {
	body, err := json.Marshal(chatRequest{
		Model:       c.model,
		Messages:    []chatMessage{{Role: "user", Content: parts}},
		MaxTokens:   maxTokens,
		Temperature: temperature,
		Plugins:     plugins,
		Usage:       &usageRequest{Include: true},
	})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	cr, err := openRouterCall(ctx, c.client, c.apiKey, body)
	if err != nil {
		return nil, err
	}
	if cr.Usage != nil {
		c.addCost(cr.Usage.Cost)
	}
	return cr, nil
}

// --- Engine registry --------------------------------------------------

// cloudRegistry maps an engine name (the provenance tag, e.g. SourceCloud)
// to a factory that builds that engine. It is the single place engine
// selection, defaulting, and the "unknown engine" error are handled, so
// adding a new cloud engine is one registry entry rather than a new switch
// case in each caller. T is the produced type — Describer for images,
// *voiceTranscriber for audio — so each modality keeps its own typed map
// while sharing the lookup logic.
type cloudRegistry[T any] struct {
	kind      string // for error messages, e.g. "media-index"
	def       string // engine used when none is requested
	factories map[string]func(apiKey, model string) (T, error)
}

// build resolves engine ("" → the registry default), then constructs it.
// An unrecognised engine is an error listing the supported names.
func (r cloudRegistry[T]) build(engine, apiKey, model string) (T, error) {
	if engine == "" {
		engine = r.def
	}
	if f, ok := r.factories[engine]; ok {
		return f(apiKey, model)
	}
	var zero T
	return zero, fmt.Errorf("%s: unknown engine %q (supported: %s)", r.kind, engine, r.names())
}

// names returns the supported engine names, sorted for a stable message.
func (r cloudRegistry[T]) names() string {
	out := make([]string, 0, len(r.factories))
	for name := range r.factories {
		out = append(out, name)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// --- OpenRouter transport ---------------------------------------------

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

// openRouterCall POSTs one chat-completion request, retrying on 429/5xx
// with exponential backoff that honours ctx cancellation. Auth/billing
// failures (401/402/403) return a FatalError so a run aborts instead of
// marking every row errored. Shared by every cloud engine. Returns the
// decoded (validated, non-empty-choices) response.
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
			// Auth / billing failures are GLOBAL, not per-request: a bad
			// key or an empty balance fails every request. Surface a
			// FatalError so the run aborts immediately instead of
			// marking thousands of rows as individually "errored".
			return nil, &FatalError{Msg: fmt.Sprintf(
				"OpenRouter rejected the request (HTTP %d): %s — check your API key and credit balance",
				resp.StatusCode, clip(respBody, 160))}
		case resp.StatusCode == http.StatusRequestEntityTooLarge:
			// The document (or a provider's image-content budget within it)
			// is too large for one request. Not transient and not global —
			// a typed error so the document extractor can split-and-retry.
			return nil, &tooLargeError{msg: clip(respBody, 160)}
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

// tooLargeError is returned for an HTTP 413 (payload / image content too
// large). The document extractor treats it as a signal to split the PDF into
// smaller page-range chunks and retry, rather than a per-row failure.
type tooLargeError struct{ msg string }

func (e *tooLargeError) Error() string { return "openrouter http 413 (too large): " + e.msg }

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
	Plugins     []pdfPlugin   `json:"plugins,omitempty"`
	Usage       *usageRequest `json:"usage,omitempty"`
}

// pdfPlugin selects OpenRouter's file-parser plugin and its PDF engine
// ("pdf-text" for the native text layer, "mistral-ocr" for scanned pages).
type pdfPlugin struct {
	ID  string          `json:"id"`
	PDF pdfPluginConfig `json:"pdf"`
}

type pdfPluginConfig struct {
	Engine string `json:"engine"`
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
	File       *filePart       `json:"file,omitempty"`
}

type imageURLPart struct {
	URL string `json:"url"`
}

// filePart carries a base64 data-URI document (PDF) for the file-parser
// plugin. FileData is "data:application/pdf;base64,<...>".
type filePart struct {
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
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
			Content     string           `json:"content"`
			Annotations []fileAnnotation `json:"annotations"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		Cost float64 `json:"cost"` // USD; present because we set usage.include
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// fileAnnotation is the file-parser plugin's parsed-document result, returned
// alongside the assistant message. The extracted text lives in
// File.Content[].Text — the faithful, length-safe source (vs. the model's
// prose reply, which can truncate a long document).
type fileAnnotation struct {
	Type string `json:"type"`
	File struct {
		Hash    string `json:"hash"`
		Name    string `json:"name"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"file"`
}
