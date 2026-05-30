package postprocess

import (
	"context"
	"fmt"
)

// A Describer turns one decrypted image into a Description. There are
// two implementations:
//
//   - appleDescriber — wraps the on-device Apple Vision subprocess
//     (the Swift `whatskept-vision` helper). macOS only.
//   - cloudDescriber — an in-process HTTP client for an OpenRouter
//     vision model. Cross-platform; the only describer on Windows.
//
// The MediaIndex loop is implementation-agnostic: it decrypts a row,
// hands the bytes to whichever Describer was selected, and persists
// the result plus its provenance (Source/Model) into wa_image_text.
type Describer interface {
	// Describe analyses one image. `path` is the on-disk decrypted
	// copy (Apple's subprocess reads the file); `data` is the same
	// bytes in memory (the cloud client base64-encodes them). A
	// per-image failure — including a model that declined — is a
	// non-nil error; the caller records it as a STATUS_ERROR row.
	Describe(ctx context.Context, rowid int64, path string, data []byte) (Description, error)

	// Source is the provenance tag stored in wa_image_text.source.
	Source() string
	// Model is the model slug stored in wa_image_text.model. Empty
	// for Apple Vision (on-device, no model id).
	Model() string

	// Close releases any held resources (the Apple subprocess; a
	// no-op for the cloud client).
	Close() error
}

// Provenance values for wa_image_text.source. Closed set — anything
// else means a row was written by an incompatible client.
const (
	SourceApple = "apple"
	SourceCloud = "cloud"
)

// FatalError marks a describe failure that is GLOBAL rather than
// per-image — a rejected API key, exhausted credits, etc. Every
// subsequent request would fail identically, so MediaIndex aborts the
// whole run on the first one instead of marking each remaining row as
// errored (which would silently churn through the entire library).
type FatalError struct{ Msg string }

func (e *FatalError) Error() string { return e.Msg }

// Description is the unified per-image result from any Describer.
// Which fields are populated depends on the source:
//
//	apple → OCRText, Language, Labels
//	cloud → OCRText, Description (Labels empty; Language best-effort)
type Description struct {
	OCRText     string        // verbatim recognized text ('' if none)
	Language    string        // dominant language/script code, may be ''
	Description string        // short summary (cloud only; '' for Apple)
	Labels      []visionLabel // classification labels (Apple only)
}

// appleDescriber adapts the existing Swift Vision worker to the
// Describer interface. It owns the subprocess and closes it.
type appleDescriber struct{ w *visionWorker }

func (a *appleDescriber) Describe(_ context.Context, rowid int64, path string, _ []byte) (Description, error) {
	vres, err := a.w.describe(rowid, path)
	if err != nil {
		return Description{}, err
	}
	if !vres.OK {
		return Description{}, fmt.Errorf("vision: %s", vres.Error)
	}
	return Description{
		OCRText:  vres.OCRText,
		Language: vres.Language,
		Labels:   vres.Labels,
	}, nil
}

func (a *appleDescriber) Source() string { return SourceApple }
func (a *appleDescriber) Model() string  { return "" }
func (a *appleDescriber) Close() error   { return a.w.Close() }
