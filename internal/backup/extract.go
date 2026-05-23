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

// ExtractChatStorage decrypts WhatsApp's ChatStorage.sqlite from the iOS
// backup at `info.Path` using `password`, and writes it to `outPath`.
//
// The parent directory of outPath is created if necessary.
//
// For unencrypted backups, password is ignored; the file is read straight
// from the manifest (not yet implemented in this Go port — encrypted only).
//
// Returns the number of bytes written.
func ExtractChatStorage(info Info, password, outPath string) (int64, error) {
	if !info.IsEncrypted {
		return 0, errors.New("unencrypted backup support not yet implemented in Go port")
	}

	udid := filepath.Base(info.Path)
	mb, err := dunhamsteve.Open(udid)
	if err != nil {
		return 0, fmt.Errorf("open backup %s: %w", udid, err)
	}
	if err := mb.SetPassword(password); err != nil {
		return 0, fmt.Errorf("unlock keybag: %w", err)
	}
	if err := mb.Load(); err != nil {
		return 0, fmt.Errorf("load manifest: %w", err)
	}

	var rec *dunhamsteve.Record
	for i := range mb.Records {
		r := &mb.Records[i]
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

	rd, err := mb.FileReader(*rec)
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
