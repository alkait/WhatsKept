package postprocess

import (
	"context"
)

// A Describer turns one decrypted image into a Description. The sole
// implementation is cloudDescriber — an in-process HTTP client for an
// OpenRouter vision model.
//
// The MediaIndex loop is implementation-agnostic: it decrypts a row,
// hands the bytes to whichever Describer was selected, and persists
// the result plus its provenance (Source/Model) into wa_image_text.
type Describer interface {
	// Describe analyses one image. `path` is the on-disk decrypted
	// copy; `data` is the same bytes in memory (the cloud client
	// base64-encodes them). A per-image failure — including a model
	// that declined — is a non-nil error; the caller records it as a
	// STATUS_ERROR row.
	Describe(ctx context.Context, rowid int64, path string, data []byte) (Description, error)

	// Source is the provenance tag stored in wa_image_text.source.
	Source() string
	// Model is the model slug stored in wa_image_text.model.
	Model() string

	// Close releases any held resources (a no-op for the cloud client).
	Close() error
}

// SourceCloud is the provenance tag written to wa_image_text.source by
// the cloud describer. Legacy rows from prior on-device runs carry
// 'cloud's complement (e.g. 'apple') and are treated as upgradeable.
const SourceCloud = "cloud"

// FatalError marks a describe failure that is GLOBAL rather than
// per-image — a rejected API key, exhausted credits, etc. Every
// subsequent request would fail identically, so MediaIndex aborts the
// whole run on the first one instead of marking each remaining row as
// errored (which would silently churn through the entire library).
type FatalError struct{ Msg string }

func (e *FatalError) Error() string { return e.Msg }

// Description is the unified per-image result from a Describer:
//
//	cloud → OCRText, Description (Language best-effort)
type Description struct {
	OCRText     string // verbatim recognized text ('' if none)
	Language    string // dominant language/script code, may be ''
	Description string // short summary
}
