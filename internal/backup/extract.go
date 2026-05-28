package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	dunhamsteve "github.com/dunhamsteve/ios/backup"
)

// WhatsApp file location inside an iOS backup.
const (
	WhatsAppDomain       = "AppDomainGroup-group.net.whatsapp.WhatsApp.shared"
	ChatStorageRelPath   = "ChatStorage.sqlite"
	defaultBackupDirName = "ChatStorage.sqlite"
)

// ErrChatStorageNotFound is returned when the backup does not contain
// WhatsApp's ChatStorage.sqlite (e.g. WhatsApp wasn't installed when the
// backup was made).
var ErrChatStorageNotFound = errors.New("ChatStorage.sqlite not found in backup")

// Record is the manifest entry for a single file inside the backup.
// Exposed as our own type so callers don't need to import the underlying
// dunhamsteve package — keep that detail behind the Bundle abstraction.
type Record = dunhamsteve.Record

// Bundle is an opened-and-unlocked iOS backup. One Bundle should be
// reused for multiple extraction passes inside the same sync, because
// the keybag-unlock step (SetPassword + Load) costs a couple of seconds
// on a heavy backup and we don't want to pay it twice.
type Bundle struct {
	mb *dunhamsteve.MobileBackup
}

// Open opens and unlocks an encrypted iOS backup. The caller is
// responsible for nothing — there is no Close; the dunhamsteve library
// holds no OS handles beyond what FileReader briefly opens.
//
// Unencrypted backups are rejected here; the rest of the pipeline
// assumes encrypted-only for now.
func Open(info Info, password string) (*Bundle, error) {
	if !info.IsEncrypted {
		return nil, errors.New("unencrypted backup support not yet implemented in Go port")
	}
	udid := filepath.Base(info.Path)
	mb, err := dunhamsteve.Open(udid)
	if err != nil {
		return nil, fmt.Errorf("open backup %s: %w", udid, err)
	}
	if err := mb.SetPassword(password); err != nil {
		return nil, fmt.Errorf("unlock keybag: %w", err)
	}
	if err := mb.Load(); err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
	}
	return &Bundle{mb: mb}, nil
}

// Records returns the full manifest. Callers should treat the slice as
// read-only — iteration order matches what the backup was written with.
func (b *Bundle) Records() []Record { return b.mb.Records }

// FileReader returns a streaming reader for the decrypted contents of a
// single file record. Caller must Close.
//
// Absorbs a quirk of the upstream `dunhamsteve/ios` library: when a
// file's decrypted payload fits in a single ~4 KB cipher block (small
// avatar thumbnails, short voice notes, tiny PDFs), `mb.FileReader`
// returns `(reader, io.EOF)` — the reader is non-nil and `io.ReadAll`
// yields the full, valid content; the EOF just signals "stream is
// already exhausted". We flatten that to `(reader, nil)` here so
// every caller can use the standard idiom
//
//	rd, err := b.FileReader(rec)
//	if err != nil { ... }
//	defer rd.Close()
//	data, err := io.ReadAll(rd)
//
// without losing valid data. Genuine errors (blob missing on disk,
// bad keybag, short read of IV, AES padding failure) surface as
// `*os.PathError` or named library errors and are unaffected.
//
// Without this absorption ~96% of `.thumb` profile pictures and any
// chat media / voice / docs that decrypt to <~4 KB are silently
// dropped — see `tmp/inspect-profile-paths/main.go`.
func (b *Bundle) FileReader(rec Record) (io.ReadCloser, error) {
	rd, err := b.mb.FileReader(rec)
	if rd != nil && errors.Is(err, io.EOF) {
		return rd, nil
	}
	return rd, err
}

// ExtractChatStorage decrypts WhatsApp's ChatStorage.sqlite from the iOS
// backup at `info.Path` using `password`, and writes it to `outPath`.
//
// The parent directory of outPath is created if necessary.
//
// Returns the number of bytes written. Thin convenience wrapper around
// Open + ExtractChatStorageFrom; use the Bundle form directly when you
// need to do further extractions in the same sync.
func ExtractChatStorage(info Info, password, outPath string) (int64, error) {
	b, err := Open(info, password)
	if err != nil {
		return 0, err
	}
	return ExtractChatStorageFrom(b, outPath)
}

// ExtractChatStorageFrom writes the ChatStorage.sqlite record from an
// already-opened Bundle to outPath. Returns the number of bytes written.
func ExtractChatStorageFrom(b *Bundle, outPath string) (int64, error) {
	var rec *Record
	for i := range b.mb.Records {
		r := &b.mb.Records[i]
		if r.Domain == WhatsAppDomain && r.Path == ChatStorageRelPath {
			rec = r
			break
		}
	}
	if rec == nil {
		return 0, ErrChatStorageNotFound
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir: %w", err)
	}

	rd, err := b.FileReader(*rec)
	if err != nil {
		return 0, fmt.Errorf("file reader: %w", err)
	}
	defer rd.Close()

	w, err := os.Create(outPath)
	if err != nil {
		return 0, fmt.Errorf("create %q: %w", outPath, err)
	}
	n, copyErr := io.Copy(w, rd)
	closeErr := w.Close()
	if copyErr != nil {
		return n, fmt.Errorf("copy: %w", copyErr)
	}
	if closeErr != nil {
		return n, fmt.Errorf("close: %w", closeErr)
	}
	if n == 0 {
		return 0, errors.New("extraction produced an empty file")
	}
	return n, nil
}
