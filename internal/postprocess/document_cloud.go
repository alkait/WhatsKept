package postprocess

// Cloud PDF text extraction via OpenRouter's file-parser plugin. A thin
// wrapper over the shared cloudClient base (cloud.go), mirroring the image
// describer and voice transcriber. The PDF is sent as a base64 file part and
// the parser engine does the work:
//
//   - "pdf-text"    free; pulls the native text layer (≈ Apple PDFKit).
//   - "mistral-ocr" $2/1000 pages; OCRs scanned pages (≈ Apple Vision OCR).
//
// We extract a document's text in two steps, mirroring the old per-page
// "native text, else OCR" logic at the document level: try pdf-text first
// (free); if it yields ~no text, the PDF is scanned, so escalate to
// mistral-ocr. The text is read from the response annotations (not the
// model's prose reply), so cost and latency are decoupled from document
// length — the carrier model only emits an ack token.
//
// Large PDFs (> ~30 MB) are rejected by the provider, so anything over a
// conservative threshold (or returning HTTP 413) is split into page-range
// chunks with pdfcpu, OCR'd per chunk, and stitched back in page order.
// Splitting is pure-Go (no cgo, no macOS frameworks), so the whole path
// works identically on macOS, Windows, and Linux.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

const (
	// DefaultDocumentModel is a cheap, large-context carrier. Extraction
	// quality comes from the file-parser engine, not the model, and we read
	// the text from annotations — so the model just needs to be inexpensive.
	DefaultDocumentModel = "google/gemini-2.5-flash-lite"

	// file-parser engines.
	enginePDFText    = "pdf-text"
	engineMistralOCR = "mistral-ocr"

	// docMaxOutputTokens is tiny on purpose: the extracted text arrives via
	// annotations, so the model only needs to emit an acknowledgement.
	docMaxOutputTokens = 8

	// docExtractPrompt is a no-op ack — the real content is the file-parser
	// annotation, not the reply.
	docExtractPrompt = "Reply with the single word: ok"

	// docEscalateBelow: if pdf-text yields fewer than this many non-space
	// content characters, treat the PDF as scanned and escalate to OCR.
	docEscalateBelow = 20

	// docInlineMaxBytes: PDFs larger than this are split before OCR. The
	// provider rejects ~30 MB documents; most WhatsApp PDFs are far smaller
	// (median ~200 KB), so this only ever touches a tiny tail.
	docInlineMaxBytes = 20 << 20

	// docChunkTargetBytes: aim each split chunk at ~this size, comfortably
	// under the provider limit.
	docChunkTargetBytes = 12 << 20

	// Provenance tags stored in wa_document_text.method.
	docMethodText  = "cloud-text"  // native text layer (pdf-text)
	docMethodOCR   = "cloud-ocr"   // OCR (mistral-ocr), possibly split
	docMethodEmpty = "empty"       // ran cleanly but found no text
)

// documentExtraction is one extractor's result for a single PDF. PagesText /
// PagesOCR are a document-level approximation of the old per-page counts
// (the cloud parser doesn't report per-page provenance): a pdf-text result
// attributes every page to text, an OCR result attributes every page to OCR.
type documentExtraction struct {
	Text      string
	Method    string
	PageCount int
	PagesText int
	PagesOCR  int
}

// cloudDocumentExtractor extracts text from one PDF per call over the shared
// cloud base. Model and CostUSD are promoted from the embedded *cloudClient.
type cloudDocumentExtractor struct {
	*cloudClient
}

// documentExtractors is the registry of supported document-index engines.
// Adding a new backend is one entry here (see cloudRegistry).
var documentExtractors = cloudRegistry[*cloudDocumentExtractor]{
	kind:      "document-index",
	def:       SourceCloud,
	factories: map[string]func(apiKey, model string) (*cloudDocumentExtractor, error){SourceCloud: newCloudDocumentExtractor},
}

func newCloudDocumentExtractor(apiKey, model string) (*cloudDocumentExtractor, error) {
	cc, err := newCloudClient(apiKey, model, DefaultDocumentModel)
	if err != nil {
		return nil, err
	}
	return &cloudDocumentExtractor{cc}, nil
}

// Source is the provenance tag stored alongside results.
func (e *cloudDocumentExtractor) Source() string { return SourceCloud }

// Extract returns the document's text. A per-document failure is a normal
// error; a global auth/billing failure is a *FatalError (from the shared
// base) that aborts the whole run.
func (e *cloudDocumentExtractor) Extract(ctx context.Context, pdf []byte, filename string) (documentExtraction, error) {
	pages := docPageCount(pdf) // best-effort; 0 if pdfcpu can't read it

	// Oversized → split up front (don't waste a doomed inline attempt).
	if len(pdf) > docInlineMaxBytes {
		return e.extractSplit(ctx, pdf, filename, pages)
	}

	// Native text layer first (free).
	raw, err := e.parseOnce(ctx, pdf, filename, enginePDFText)
	if err == nil && len(stripPDFNoise(raw)) >= docEscalateBelow {
		return documentExtraction{
			Text: cleanParsed(raw), Method: docMethodText,
			PageCount: pages, PagesText: pages,
		}, nil
	}

	// Scanned (or pdf-text failed to parse): OCR.
	ocrRaw, ocrErr := e.parseOnce(ctx, pdf, filename, engineMistralOCR)
	if ocrErr != nil {
		var tooLarge *tooLargeError
		if errors.As(ocrErr, &tooLarge) {
			return e.extractSplit(ctx, pdf, filename, pages)
		}
		// OCR failed but pdf-text returned *some* text — keep it rather
		// than failing the row.
		if err == nil {
			if t := cleanParsed(raw); t != "" {
				return documentExtraction{Text: t, Method: docMethodText, PageCount: pages, PagesText: pages}, nil
			}
		}
		return documentExtraction{}, ocrErr
	}
	cleaned := cleanParsed(ocrRaw)
	if cleaned == "" {
		return documentExtraction{Method: docMethodEmpty, PageCount: pages}, nil
	}
	return documentExtraction{Text: cleaned, Method: docMethodOCR, PageCount: pages, PagesOCR: pages}, nil
}

// extractSplit splits an oversized PDF into page-range chunks (each under the
// provider limit), OCRs every chunk, and stitches the text in page order.
func (e *cloudDocumentExtractor) extractSplit(ctx context.Context, pdf []byte, filename string, pages int) (documentExtraction, error) {
	chunks, err := splitPDF(pdf, pages)
	if err != nil {
		return documentExtraction{}, fmt.Errorf("split oversized PDF: %w", err)
	}
	var parts []string
	for i, ch := range chunks {
		raw, err := e.parseOnce(ctx, ch, fmt.Sprintf("%s.part%d.pdf", filename, i+1), engineMistralOCR)
		if err != nil {
			return documentExtraction{}, fmt.Errorf("OCR chunk %d/%d: %w", i+1, len(chunks), err)
		}
		if t := cleanParsed(raw); t != "" {
			parts = append(parts, t)
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if text == "" {
		return documentExtraction{Method: docMethodEmpty, PageCount: pages}, nil
	}
	return documentExtraction{Text: text, Method: docMethodOCR, PageCount: pages, PagesOCR: pages}, nil
}

// parseOnce sends one PDF through one file-parser engine and returns the
// joined annotation text.
func (e *cloudDocumentExtractor) parseOnce(ctx context.Context, pdf []byte, filename, engine string) (string, error) {
	dataURI := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdf)
	cr, err := e.completeRaw(ctx, []contentPart{
		{Type: "text", Text: docExtractPrompt},
		{Type: "file", File: &filePart{Filename: filename, FileData: dataURI}},
	}, []pdfPlugin{{ID: "file-parser", PDF: pdfPluginConfig{Engine: engine}}}, docMaxOutputTokens, 0)
	if err != nil {
		return "", err
	}
	return annotationText(cr), nil
}

// annotationText joins the file-parser annotation content segments, dropping
// the "<file name=...>" / "</file>" wrapper segments.
func annotationText(cr *chatResponse) string {
	if len(cr.Choices) == 0 {
		return ""
	}
	var b strings.Builder
	for _, a := range cr.Choices[0].Message.Annotations {
		if a.Type != "file" {
			continue
		}
		for _, seg := range a.File.Content {
			t := seg.Text
			if strings.HasPrefix(t, "<file ") || strings.TrimSpace(t) == "</file>" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(t)
		}
	}
	return strings.TrimSpace(b.String())
}

// cleanParsed strips the pdf-text "# file … ## Metadata … ## Contents"
// header so we store just the body. mistral-ocr output has no such header and
// passes through unchanged.
func cleanParsed(s string) string {
	if i := strings.Index(s, "## Contents"); i >= 0 {
		s = s[i+len("## Contents"):]
	}
	return strings.TrimSpace(s)
}

// stripPDFNoise reduces a pdf-text result to its bare content characters
// (no headings, page markers, or whitespace) so the escalation check can tell
// "has a real text layer" from "scanned, parser only emitted structure".
func stripPDFNoise(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(cleanParsed(s), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		b.WriteString(t)
	}
	return b.String()
}

// docPageCount returns the PDF's page count via pdfcpu, or 0 if it can't be
// read. Best-effort: the page count is metadata, never the gate on extraction.
func docPageCount(pdf []byte) int {
	n, err := api.PageCount(bytes.NewReader(pdf), model.NewDefaultConfiguration())
	if err != nil {
		return 0
	}
	return n
}

// splitPDF cuts a PDF into page-range chunks each ≈ docChunkTargetBytes, sized
// from the document's bytes-per-page so image-heavy scans split finely enough
// to clear the provider limit. Pure-Go (pdfcpu), so it runs anywhere.
func splitPDF(pdf []byte, pages int) ([][]byte, error) {
	if pages <= 0 {
		if pages = docPageCount(pdf); pages <= 0 {
			return nil, errors.New("cannot determine page count")
		}
	}
	bytesPerPage := len(pdf) / pages
	if bytesPerPage < 1 {
		bytesPerPage = 1
	}
	span := docChunkTargetBytes / bytesPerPage
	if span < 1 {
		span = 1
	}
	conf := model.NewDefaultConfiguration()
	var chunks [][]byte
	for start := 1; start <= pages; start += span {
		end := start + span - 1
		if end > pages {
			end = pages
		}
		var buf bytes.Buffer
		rng := fmt.Sprintf("%d-%d", start, end)
		if err := api.Trim(bytes.NewReader(pdf), &buf, []string{rng}, conf); err != nil {
			return nil, fmt.Errorf("trim pages %s: %w", rng, err)
		}
		chunks = append(chunks, append([]byte(nil), buf.Bytes()...))
	}
	return chunks, nil
}
