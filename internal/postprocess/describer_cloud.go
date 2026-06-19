package postprocess

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
)

// cloudDescriber describes images via an OpenRouter vision model. It is a
// thin wrapper over the shared cloudClient base (cloud.go): the base owns
// the HTTP transport, retries, cost accounting, and model bookkeeping;
// this file only supplies the prompt, the image content part, and how to
// split the reply into OCR + description.
//
// One model call per image returns a structured reply that we split into
// verbatim OCR (wa_image_text.ocr_text) and a short summary
// (wa_image_text.description); see cloudDescribePrompt and
// splitTextDescription.

const (
	// DefaultCloudModel is a cheap, strong multilingual-OCR default.
	DefaultCloudModel = "qwen/qwen3-vl-8b-instruct"

	cloudMaxOutputTokens = 700

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

// cloudDescriber implements Describer over the shared cloud base. Model,
// CostUSD, and Close are promoted from the embedded *cloudClient.
type cloudDescriber struct {
	*cloudClient
}

// imageDescribers is the registry of supported media-index engines. Adding
// a new vision backend is one entry here (see cloudRegistry). Cloud is the
// only engine today and the default.
var imageDescribers = cloudRegistry[Describer]{
	kind: "media-index",
	def:  SourceCloud,
	factories: map[string]func(apiKey, model string) (Describer, error){
		SourceCloud: func(apiKey, model string) (Describer, error) {
			return newCloudDescriber(apiKey, model)
		},
	},
}

// newCloudDescriber builds a cloud describer. An empty model falls back to
// DefaultCloudModel; an empty apiKey is an error.
func newCloudDescriber(apiKey, model string) (*cloudDescriber, error) {
	cc, err := newCloudClient(apiKey, model, DefaultCloudModel)
	if err != nil {
		return nil, err
	}
	return &cloudDescriber{cc}, nil
}

// Source is the provenance tag stored in wa_image_text.source.
func (c *cloudDescriber) Source() string { return SourceCloud }

func (c *cloudDescriber) Describe(ctx context.Context, _ int64, _ string, data []byte) (Description, error) {
	ext, ok := detectImageFormat(data)
	if !ok {
		return Description{}, errors.New("unrecognized image format")
	}
	dataURI := "data:" + mimeForImageExt(ext) + ";base64," + base64.StdEncoding.EncodeToString(data)

	raw, err := c.complete(ctx, []contentPart{
		{Type: "text", Text: cloudDescribePrompt},
		{Type: "image_url", ImageURL: &imageURLPart{URL: dataURI}},
	}, cloudMaxOutputTokens, 0.1)
	if err != nil {
		return Description{}, err
	}
	ocr, desc := splitTextDescription(raw)
	return Description{OCRText: ocr, Description: desc}, nil
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
