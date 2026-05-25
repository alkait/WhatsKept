package postprocess

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"whatskept/internal/backup"
	"whatskept/internal/helpers"
)

// This file is the Go port of the Python whatskept.voice_indexer
// module. It owns one user-visible operation:
//
//   `whatskept voice-index` (CLI) / "Sync voice notes" (GUI):
//   walk every WhatsApp .opus voice-note message in ChatStorage.sqlite,
//   decrypt the OPUS from the iOS backup, transcribe via the bundled
//   whisper-cli (Metal-accelerated on Apple Silicon), and persist
//   transcripts in wa_voice_text. Resumable per-row.
//
// Pipeline per clip:
//
//   1. Look up backup-manifest record by ZMEDIALOCALPATH.
//   2. Decrypt OPUS bytes to memory and write to
//      <workspace>/voice/<rowid>.opus.
//   3. Run /usr/bin/afconvert OPUS -> 16 kHz mono Int16 WAV
//      (whisper-cli's required input format).
//   4. Run whisper-cli with -l auto -oj, parse the JSON output.
//   5. Atomically write voice_index + wa_voice_text rows.
//   6. Delete the temporary WAV (kept only between steps 3 and 4).
//
// Schema is shared with sidecar.go's createVoiceSidecarsSQL:
//
//   wa_voice_text(rowid, transcript, language, duration_sec,
//                 segments_json, generated_at)
//   voice_index  (rowid, manifest_path, status, bytes,
//                 duration_sec, error, attempted_at)
//
// Why no concurrency: whisper-cli is GPU-bound on Metal; running
// two clips in parallel costs more in GPU contention than it saves
// in wall time. Measured throughput on M-series is ~14× realtime
// single-stream, which is fast enough that a typical user's
// voice-note backlog (~1 hour total audio) finishes in ~5 minutes.
// We can revisit if profiling shows GPU underutilisation.

const (
	// Status values stored in voice_index.status.
	VoiceStatusTranscribed = "transcribed"
	VoiceStatusMissing     = "missing"
	VoiceStatusError       = "error"

	// Manifest-path prefix for WhatsApp media (same as image side).
	voiceManifestPrefix = "Message/"

	// OPUS magic at the start of an Ogg-encapsulated stream.
	oggMagic = "OggS"
)

// VoiceIndexOptions configures one VoiceIndex run. Mirrors
// MediaIndexOptions for cross-call symmetry; see media.go for
// shared field semantics.
type VoiceIndexOptions struct {
	// Workspace is the directory containing ChatStorage.sqlite.
	// Decrypted OPUS files land in <Workspace>/voice/<rowid>.opus.
	Workspace string

	// BackupPath / BackupRoot — optional override and search root
	// for the backup discovery; identical semantics to MediaIndex.
	BackupPath string
	BackupRoot string

	// Password to unlock the encrypted backup. Required.
	Password string

	// Language pins transcription to one BCP-47 / ISO-639-1 code
	// (e.g. "ar", "en"). Empty string (default) means whisper-cli
	// auto-detects per clip — what you want when chats mix Arabic
	// and English voice notes from different senders.
	Language string

	// Limit caps the number of rows attempted in this run. 0 =
	// unlimited.
	Limit int

	// RetryMissing / RetryErrors — re-attempt rows previously
	// marked terminal. Same semantics as media-index.
	RetryMissing bool
	RetryErrors  bool

	// Ctx is checked between rows for graceful cancellation.
	Ctx context.Context

	// Log receives one-line human-readable progress lines.
	Log func(string)

	// Progress receives a structured update every ProgressEvery
	// rows. Default 1 — voice notes are slow enough (seconds per
	// clip) that batching progress would feel laggy.
	Progress      func(VoiceIndexProgress)
	ProgressEvery int
}

// VoiceIndexProgress is what callers see during a run.
type VoiceIndexProgress struct {
	Done         int     `json:"done"`
	Total        int     `json:"total"`
	Pending      int     `json:"pending"`
	Transcribed  int     `json:"transcribed"`
	Missing      int     `json:"missing"`
	Errors       int     `json:"errors"`
	CurrentLabel string  `json:"current_label,omitempty"` // "rowid=12345 dur=42s"
	RatePerSec   float64 `json:"rate_per_sec"`
	ETASeconds   float64 `json:"eta_seconds"`
	ElapsedSec   float64 `json:"elapsed_sec"`
}

// VoiceIndexResult is the final stats summary.
type VoiceIndexResult struct {
	BackupPath         string  `json:"backup_path"`
	Workspace          string  `json:"workspace"`
	TotalCandidates    int     `json:"total_candidates"`
	AlreadyTranscribed int     `json:"already_transcribed"`
	Processed          int     `json:"processed"`
	Transcribed        int     `json:"transcribed"`
	Missing            int     `json:"missing"`
	Errors             int     `json:"errors"`
	DurationSec        float64 `json:"duration_sec"`
	AudioSecondsTotal  float64 `json:"audio_seconds_total"` // sum of clip durations transcribed
	FTSCountAfter      int     `json:"fts_count_after"`
	Cancelled          bool    `json:"cancelled,omitempty"`
}

// ErrModelNotInstalled is the sentinel returned when the speech
// model isn't on disk. Callers route this to the "first-run
// download" UX rather than treating it as a bug.
var ErrModelNotInstalled = errors.New("whisper model not installed; download required before voice-index")

// VoiceIndex runs the voice-transcription pipeline against the
// workspace's ChatStorage.sqlite + most-recent encrypted backup.
func VoiceIndex(opts VoiceIndexOptions) (*VoiceIndexResult, error) {
	if opts.Workspace == "" {
		return nil, errors.New("voice-index: Workspace required")
	}
	if opts.Password == "" {
		return nil, errors.New("voice-index: Password required for encrypted backups")
	}
	if opts.Ctx == nil {
		opts.Ctx = context.Background()
	}
	if opts.Log == nil {
		opts.Log = func(string) {}
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = 1
	}

	// --- Verify the speech model is installed -------------------
	// We do a size-only check here; full sha256 verification is
	// quite slow (~0.5s for 574MB) and the trust window is short
	// — the file just sits in Application Support, untouched
	// between runs. The download path always sha-verifies after
	// fetch, which is when corruption is most likely.
	modelStatus, _, err := helpers.CheckModel(helpers.WhisperModel, false)
	if err != nil {
		return nil, fmt.Errorf("check model: %w", err)
	}
	if modelStatus != helpers.ModelPresent && modelStatus != helpers.ModelVerified {
		return nil, ErrModelNotInstalled
	}
	modelPath, err := helpers.ModelPath(helpers.WhisperModel)
	if err != nil {
		return nil, fmt.Errorf("model path: %w", err)
	}

	// --- Resolve whisper-cli ------------------------------------
	whisperBin, err := resolveWhisperCli()
	if err != nil {
		return nil, fmt.Errorf("locate whisper-cli: %w", err)
	}

	// --- Set up workspace dirs ----------------------------------
	dbPath := filepath.Join(opts.Workspace, "ChatStorage.sqlite")
	voiceDir := filepath.Join(opts.Workspace, "voice")
	if err := os.MkdirAll(voiceDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir voice dir: %w", err)
	}

	// --- Open DB + ensure schema --------------------------------
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec(createVoiceSidecarsSQL); err != nil {
		return nil, fmt.Errorf("create voice sidecar tables: %w", err)
	}

	// --- Locate and unlock the backup ---------------------------
	info, err := pickVoiceIndexBackup(opts)
	if err != nil {
		return nil, err
	}
	if !info.IsEncrypted {
		return nil, errors.New("voice-index: backup is not encrypted (whatskept voice-index requires an encrypted backup)")
	}
	opts.Log(fmt.Sprintf("Backup: %s", info.Path))
	opts.Log("Unlocking iOS backup…")
	bundle, err := backup.Open(info, opts.Password)
	if err != nil {
		return nil, fmt.Errorf("open backup: %w", err)
	}

	opts.Log("Indexing backup manifest…")
	manifestIdx := buildManifestIndex(bundle, backup.WhatsAppDomain)

	// --- Pick candidates ---------------------------------------
	total, alreadyTranscribed, err := countVoiceCandidates(db)
	if err != nil {
		return nil, err
	}
	candidates, err := selectVoiceCandidates(db, opts.RetryMissing, opts.RetryErrors, opts.Limit)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		opts.Log(fmt.Sprintf("Nothing to do: %d / %d already transcribed.", alreadyTranscribed, total))
		ftsN, _ := rebuildFTS(db)
		return &VoiceIndexResult{
			BackupPath:         info.Path,
			Workspace:          opts.Workspace,
			TotalCandidates:    total,
			AlreadyTranscribed: alreadyTranscribed,
			FTSCountAfter:      ftsN,
		}, nil
	}
	opts.Log(fmt.Sprintf("Voice notes on device:    %d", total))
	opts.Log(fmt.Sprintf("Already transcribed:      %d", alreadyTranscribed))
	opts.Log(fmt.Sprintf("Queued this run:          %d", len(candidates)))
	opts.Log(fmt.Sprintf("Engine: whisper-cli (%s)", helpers.WhisperModel.Name))

	// --- Main loop ---------------------------------------------
	res := &VoiceIndexResult{
		BackupPath:         info.Path,
		Workspace:          opts.Workspace,
		TotalCandidates:    total,
		AlreadyTranscribed: alreadyTranscribed,
	}
	tStart := time.Now()

	for i, c := range candidates {
		select {
		case <-opts.Ctx.Done():
			res.Cancelled = true
			opts.Log("Stopped on user request. All committed rows are safe; re-run to resume.")
			res.DurationSec = time.Since(tStart).Seconds()
			return res, nil
		default:
		}

		status, audioSec := processOneVoice(opts.Ctx, db, bundle, manifestIdx, voiceDir,
			whisperBin, modelPath, opts.Language, c, opts.Log)
		res.Processed++
		switch status {
		case VoiceStatusTranscribed:
			res.Transcribed++
			res.AudioSecondsTotal += audioSec
		case VoiceStatusMissing:
			res.Missing++
		case VoiceStatusError:
			res.Errors++
		}

		if opts.Progress != nil && (i+1)%opts.ProgressEvery == 0 {
			emitVoiceProgress(opts.Progress, res, total, alreadyTranscribed, len(candidates), tStart, c)
		}
	}

	res.DurationSec = time.Since(tStart).Seconds()
	if opts.Progress != nil {
		emitVoiceProgress(opts.Progress, res, total, alreadyTranscribed, len(candidates), tStart, voiceCandidate{})
	}

	opts.Log(fmt.Sprintf(
		"Done in %.0fs. transcribed=%d missing=%d errors=%d audio=%.0fs (rate %.1f×rt)",
		res.DurationSec, res.Transcribed, res.Missing, res.Errors,
		res.AudioSecondsTotal,
		res.AudioSecondsTotal/maxF(res.DurationSec, 0.001),
	))

	// Rebuild FTS so transcripts become searchable alongside images / messages.
	opts.Log("Rebuilding messages_fts…")
	ftsN, err := rebuildFTS(db)
	if err != nil {
		opts.Log(fmt.Sprintf("FTS rebuild failed (non-fatal): %v", err))
	} else {
		res.FTSCountAfter = ftsN
		opts.Log(fmt.Sprintf("messages_fts: %d rows indexed.", ftsN))
	}

	return res, nil
}

// pickVoiceIndexBackup mirrors pickMediaIndexBackup in media.go;
// kept separate so a future divergence in either path doesn't
// require an awkward refactor.
func pickVoiceIndexBackup(opts VoiceIndexOptions) (backup.Info, error) {
	mo := MediaIndexOptions{
		BackupPath: opts.BackupPath,
		BackupRoot: opts.BackupRoot,
	}
	return pickMediaIndexBackup(mo)
}

// voiceCandidate is one row we'll try to transcribe.
type voiceCandidate struct {
	rowid        int64
	manifestPath string  // 'Message/Media/.../foo.opus'
	durationSec  float64 // from ZMOVIEDURATION (whisper.cpp doesn't need it but it's a useful UI hint)
}

func (c voiceCandidate) label() string {
	if c.rowid == 0 {
		return ""
	}
	if c.durationSec > 0 {
		return fmt.Sprintf("rowid=%d dur=%.0fs", c.rowid, c.durationSec)
	}
	return fmt.Sprintf("rowid=%d", c.rowid)
}

// countVoiceCandidates returns (total_in_db, already_transcribed).
func countVoiceCandidates(db *sql.DB) (total, already int, err error) {
	if err = db.QueryRow(
		`SELECT COUNT(*)
		 FROM ZWAMEDIAITEM m
		 JOIN ZWAMESSAGE wm ON wm.Z_PK = m.ZMESSAGE
		 WHERE m.ZMEDIALOCALPATH LIKE '%.opus'`,
	).Scan(&total); err != nil {
		return 0, 0, fmt.Errorf("count total: %w", err)
	}
	if err = db.QueryRow(
		`SELECT COUNT(*) FROM voice_index WHERE status = ?`,
		VoiceStatusTranscribed,
	).Scan(&already); err != nil {
		return 0, 0, fmt.Errorf("count transcribed: %w", err)
	}
	return total, already, nil
}

// selectVoiceCandidates is the resume query: rows with a "skip"
// status are excluded. Results are ordered by ascending rowid so
// each run produces deterministic progress events.
func selectVoiceCandidates(db *sql.DB, retryMissing, retryErrors bool, limit int) ([]voiceCandidate, error) {
	skip := []any{VoiceStatusTranscribed}
	if !retryMissing {
		skip = append(skip, VoiceStatusMissing)
	}
	if !retryErrors {
		skip = append(skip, VoiceStatusError)
	}
	placeholders := make([]byte, 0, len(skip)*2)
	for i := range skip {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
	}

	q := fmt.Sprintf(`
		SELECT m.ZMESSAGE,
		       '%s' || m.ZMEDIALOCALPATH,
		       COALESCE(m.ZMOVIEDURATION, 0)
		FROM   ZWAMEDIAITEM m
		JOIN   ZWAMESSAGE   wm ON wm.Z_PK = m.ZMESSAGE
		WHERE  m.ZMEDIALOCALPATH LIKE '%%.opus'
		  AND  m.ZMESSAGE NOT IN (
		         SELECT rowid FROM voice_index WHERE status IN (%s)
		       )
		ORDER BY m.ZMESSAGE ASC`,
		voiceManifestPrefix, string(placeholders))
	if limit > 0 {
		q += fmt.Sprintf("\n\t\tLIMIT %d", limit)
	}

	rows, err := db.Query(q, skip...)
	if err != nil {
		return nil, fmt.Errorf("select voice candidates: %w", err)
	}
	defer rows.Close()

	var out []voiceCandidate
	for rows.Next() {
		var c voiceCandidate
		if err := rows.Scan(&c.rowid, &c.manifestPath, &c.durationSec); err != nil {
			return nil, fmt.Errorf("scan voice candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, nil
}

// processOneVoice runs the full decrypt → convert → transcribe →
// commit pipeline for one candidate. Returns (status, audioSec)
// where audioSec is the clip duration successfully transcribed
// (so the caller can sum total throughput); 0 on any non-success.
func processOneVoice(
	ctx context.Context,
	db *sql.DB,
	bundle *backup.Bundle,
	manifestIdx map[string]*backup.Record,
	voiceDir, whisperBin, modelPath, language string,
	c voiceCandidate,
	log func(string),
) (string, float64) {
	now := nowUTC()

	// 1. Manifest lookup.
	rec, ok := manifestIdx[c.manifestPath]
	if !ok {
		writeVoiceIndex(db, c, VoiceStatusMissing, 0, "", now)
		return VoiceStatusMissing, 0
	}

	// 2. Decrypt to memory. EOF here is its own thing: the manifest
	//    references the blob but the iOS backup didn't actually
	//    persist its bytes (typical for newer voice notes the user
	//    hasn't opened on the phone since the last backup). It's
	//    indistinguishable from "missing from manifest" from the
	//    user's POV, so classify it as missing rather than error.
	rd, err := bundle.FileReader(*rec)
	if err != nil {
		if errors.Is(err, io.EOF) {
			writeVoiceIndex(db, c, VoiceStatusMissing, 0, "", now)
			return VoiceStatusMissing, 0
		}
		writeVoiceIndex(db, c, VoiceStatusError, 0,
			fmt.Sprintf("decrypt: %v", err), now)
		return VoiceStatusError, 0
	}
	data, err := io.ReadAll(rd)
	_ = rd.Close()
	if err != nil {
		if errors.Is(err, io.EOF) {
			writeVoiceIndex(db, c, VoiceStatusMissing, 0, "", now)
			return VoiceStatusMissing, 0
		}
		writeVoiceIndex(db, c, VoiceStatusError, int64(len(data)),
			fmt.Sprintf("read: %v", err), now)
		return VoiceStatusError, 0
	}

	// 3. Sanity-check Ogg magic.
	if len(data) < len(oggMagic) || string(data[:len(oggMagic)]) != oggMagic {
		writeVoiceIndex(db, c, VoiceStatusError, int64(len(data)),
			"not an Ogg/OPUS file", now)
		return VoiceStatusError, 0
	}

	// 4. Persist OPUS to disk (so the agent can replay it).
	opusPath := filepath.Join(voiceDir, fmt.Sprintf("%d.opus", c.rowid))
	if err := writeFileAtomic(opusPath, data); err != nil {
		writeVoiceIndex(db, c, VoiceStatusError, int64(len(data)),
			fmt.Sprintf("write opus: %v", err), now)
		return VoiceStatusError, 0
	}

	// 5. Convert OPUS -> WAV (16 kHz mono Int16 — what whisper-cli
	//    wants). WAV is temporary; deleted on the success path.
	wavPath := filepath.Join(voiceDir, fmt.Sprintf("%d.wav", c.rowid))
	if err := convertOpusToWav(ctx, opusPath, wavPath); err != nil {
		writeVoiceIndex(db, c, VoiceStatusError, int64(len(data)),
			fmt.Sprintf("convert: %v", err), now)
		_ = os.Remove(wavPath)
		return VoiceStatusError, 0
	}
	defer os.Remove(wavPath) // success or failure, the WAV is disposable

	// 6. Transcribe via whisper-cli.
	tres, err := runWhisper(ctx, whisperBin, modelPath, wavPath, language)
	if err != nil {
		writeVoiceIndex(db, c, VoiceStatusError, int64(len(data)),
			fmt.Sprintf("whisper: %v", err), now)
		return VoiceStatusError, 0
	}

	// 7. Persist results atomically.
	tx, err := db.Begin()
	if err != nil {
		log(fmt.Sprintf("[rowid=%d] begin tx: %v", c.rowid, err))
		return VoiceStatusError, 0
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO wa_voice_text
		 (rowid, transcript, language, duration_sec, segments_json, generated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.rowid, tres.Transcript, tres.Language, c.durationSec, tres.SegmentsJSON, now,
	); err != nil {
		_ = tx.Rollback()
		log(fmt.Sprintf("[rowid=%d] insert wa_voice_text: %v", c.rowid, err))
		return VoiceStatusError, 0
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO voice_index
		 (rowid, manifest_path, status, bytes, duration_sec, error, attempted_at)
		 VALUES (?, ?, ?, ?, ?, NULL, ?)`,
		c.rowid, c.manifestPath, VoiceStatusTranscribed,
		int64(len(data)), c.durationSec, now,
	); err != nil {
		_ = tx.Rollback()
		log(fmt.Sprintf("[rowid=%d] insert voice_index: %v", c.rowid, err))
		return VoiceStatusError, 0
	}
	if err := tx.Commit(); err != nil {
		log(fmt.Sprintf("[rowid=%d] commit: %v", c.rowid, err))
		return VoiceStatusError, 0
	}

	return VoiceStatusTranscribed, c.durationSec
}

// writeVoiceIndex commits a single voice_index row outside any
// caller-managed transaction. Used for terminal-status writes
// where there's no corresponding wa_voice_text row to keep atomic.
func writeVoiceIndex(db *sql.DB, c voiceCandidate, status string, bytesLen int64, errMsg string, now string) {
	var errVal any
	if errMsg != "" {
		errVal = errMsg
	}
	_, _ = db.Exec(
		`INSERT OR REPLACE INTO voice_index
		 (rowid, manifest_path, status, bytes, duration_sec, error, attempted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.rowid, c.manifestPath, status, bytesLen, c.durationSec, errVal, now,
	)
}

// convertOpusToWav shells out to /usr/bin/afconvert (bundled with
// macOS) to produce a 16 kHz mono Int16 WAV from an Ogg/OPUS source.
// We don't use AVAudioConverter from cgo because afconvert is more
// robust for arbitrary OPUS streams (tested in the Phase-0b POC)
// and adding cgo just for this would be a regression.
func convertOpusToWav(ctx context.Context, src, dst string) error {
	// Remove any stale destination first so afconvert never opens
	// an existing file in append mode.
	_ = os.Remove(dst)

	cmd := exec.CommandContext(ctx, "/usr/bin/afconvert",
		"-f", "WAVE",
		"-d", "LEI16@16000", // little-endian Int16, 16 kHz
		"-c", "1", // mono
		src, dst,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// afconvert prints errors to stderr; trim trailing newline.
		msg := strings.TrimSpace(string(out))
		return fmt.Errorf("afconvert failed: %v: %s", err, msg)
	}
	return nil
}

// transcribeResult bundles what we extract from whisper-cli's JSON.
type transcribeResult struct {
	Transcript   string
	Language     string
	SegmentsJSON string // compact JSON, ready to write to wa_voice_text
}

// whisperJSONOutput models the parts of whisper-cli's --output-json
// schema we care about. There's more (model metadata, system_info,
// per-token probabilities) but they're useless for our purposes.
type whisperJSONOutput struct {
	Result struct {
		Language string `json:"language"`
	} `json:"result"`
	Transcription []whisperSegment `json:"transcription"`
}

type whisperSegment struct {
	Timestamps struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"timestamps"`
	Offsets struct {
		From int `json:"from"`
		To   int `json:"to"`
	} `json:"offsets"`
	Text string `json:"text"`
}

// runWhisper invokes whisper-cli on a prepared WAV and returns the
// extracted transcript + language. The CLI writes its JSON to a
// sibling file at `<basename>.json`; we read and delete it.
func runWhisper(ctx context.Context, whisperBin, modelPath, wavPath, language string) (transcribeResult, error) {
	jsonPath := strings.TrimSuffix(wavPath, ".wav") + ".json"
	_ = os.Remove(jsonPath) // belt-and-braces

	args := []string{
		"-m", modelPath,
		"-f", wavPath,
		"-oj",                                      // output JSON
		"-of", strings.TrimSuffix(wavPath, ".wav"), // -of strips its own .json suffix
		"-nt", // no per-segment timestamps in the textual output
	}
	if language == "" {
		args = append(args, "-l", "auto")
	} else {
		args = append(args, "-l", language)
	}

	cmd := exec.CommandContext(ctx, whisperBin, args...)
	// whisper-cli prints all its progress / model-load chatter to
	// stderr; we don't need any of it on a happy path. CombinedOutput
	// would gobble it for the error message, which is what we want.
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Trim noise so the DB error column stays useful.
		tail := tailLine(out, 256)
		return transcribeResult{}, fmt.Errorf("%v: %s", err, tail)
	}
	defer os.Remove(jsonPath)

	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		return transcribeResult{}, fmt.Errorf("read whisper json: %w", err)
	}
	var parsed whisperJSONOutput
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return transcribeResult{}, fmt.Errorf("parse whisper json: %w", err)
	}

	// Concatenate segment texts, trim leading whitespace per
	// segment (whisper.cpp reliably emits a leading space).
	var sb strings.Builder
	for _, s := range parsed.Transcription {
		sb.WriteString(strings.TrimSpace(s.Text))
		sb.WriteByte(' ')
	}
	transcript := strings.TrimSpace(sb.String())

	// Build a compact segments JSON for storage. Drop fields we
	// don't actually use (`offsets.{from,to}` are ms duplicates
	// of the timestamp strings).
	type compactSeg struct {
		From string `json:"from"`
		To   string `json:"to"`
		Text string `json:"text"`
	}
	compact := make([]compactSeg, 0, len(parsed.Transcription))
	for _, s := range parsed.Transcription {
		compact = append(compact, compactSeg{
			From: s.Timestamps.From,
			To:   s.Timestamps.To,
			Text: strings.TrimSpace(s.Text),
		})
	}
	segJSON, _ := json.Marshal(compact)

	return transcribeResult{
		Transcript:   transcript,
		Language:     parsed.Result.Language,
		SegmentsJSON: string(segJSON),
	}, nil
}

// tailLine returns the last non-empty line of `out`, truncated to
// `max` bytes. Useful for surfacing whisper-cli's actual error
// message in the DB error column when the binary failed.
func tailLine(out []byte, max int) string {
	s := strings.TrimRight(string(out), "\r\n\t ")
	if i := strings.LastIndexAny(s, "\r\n"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// resolveWhisperCli returns the absolute path to the extracted
// whisper-cli binary. Mirrors resolveVisionHelper in media.go.
func resolveWhisperCli() (string, error) {
	dir, err := helpers.Path()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, helpers.WhisperCli)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("helper not found at %s: %w", p, err)
	}
	return p, nil
}

// emitVoiceProgress fills in the rate / ETA fields and invokes the
// caller's progress callback.
func emitVoiceProgress(cb func(VoiceIndexProgress), res *VoiceIndexResult, total, already, pending int, started time.Time, current voiceCandidate) {
	if cb == nil {
		return
	}
	elapsed := time.Since(started).Seconds()
	rate := float64(res.Processed) / maxF(elapsed, 0.001)
	remaining := pending - res.Processed
	eta := 0.0
	if rate > 0 && remaining > 0 {
		eta = float64(remaining) / rate
	}
	cb(VoiceIndexProgress{
		Done:         res.Processed,
		Total:        total,
		Pending:      pending,
		Transcribed:  res.Transcribed,
		Missing:      res.Missing,
		Errors:       res.Errors,
		CurrentLabel: current.label(),
		RatePerSec:   rate,
		ETASeconds:   eta,
		ElapsedSec:   elapsed,
	})
}
