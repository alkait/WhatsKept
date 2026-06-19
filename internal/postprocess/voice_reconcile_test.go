package postprocess

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// seedVoiceReconcileDB builds an in-memory ChatStorage-shaped DB with three
// .opus voice messages plus one non-voice message, and an empty voice_index —
// mirroring the post-re-sync state where the DB was rebuilt but the on-disk
// voice/ files survived.
func seedVoiceReconcileDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed exec %q: %v", q, err)
		}
	}
	exec(`CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZMESSAGETYPE INTEGER)`)
	exec(`CREATE TABLE ZWAMEDIAITEM (ZMESSAGE INTEGER, ZMEDIALOCALPATH TEXT, ZMOVIEDURATION REAL)`)
	if err := ensureVoiceSidecarSchema(db); err != nil {
		t.Fatal(err)
	}
	// rowids 1-3 are .opus voice notes; rowid 4 is an image (must be ignored).
	for i := 1; i <= 3; i++ {
		exec(`INSERT INTO ZWAMESSAGE (Z_PK, ZMESSAGETYPE) VALUES (?, 5)`, i)
		exec(`INSERT INTO ZWAMEDIAITEM (ZMESSAGE, ZMEDIALOCALPATH, ZMOVIEDURATION) VALUES (?, ?, ?)`,
			i, fmt.Sprintf("Media/%d.opus", i), float64(i))
	}
	exec(`INSERT INTO ZWAMESSAGE (Z_PK, ZMESSAGETYPE) VALUES (4, 5)`)
	exec(`INSERT INTO ZWAMEDIAITEM (ZMESSAGE, ZMEDIALOCALPATH, ZMOVIEDURATION) VALUES (4, 'Media/4.jpg', 0)`)
	return db
}

func TestReconcileVoiceIndexFromDisk(t *testing.T) {
	db := seedVoiceReconcileDB(t)
	voiceDir := t.TempDir()

	// On disk: clips 1,2,3 (real voice notes), 99 (orphan with no media item),
	// plus a stray non-.opus file and the image rowid 4 — none of the latter
	// should be adopted.
	for _, name := range []string{"1.opus", "2.opus", "3.opus", "99.opus", "4.opus", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(voiceDir, name), []byte("ogg"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Clip 2 is already tracked as transcribed — reconcile must not touch it.
	if _, err := db.Exec(
		`INSERT INTO voice_index (rowid, manifest_path, status, bytes, duration_sec, attempted_at)
		 VALUES (2, 'Message/Media/2.opus', ?, 3, 2, '2020-01-01T00:00:00Z')`,
		VoiceStatusTranscribed,
	); err != nil {
		t.Fatal(err)
	}

	n, err := reconcileVoiceIndexFromDisk(db, voiceDir)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Only clips 1 and 3 are adopted: 2 is already indexed, 99 has no media
	// item, 4 is an image, notes.txt is not a clip.
	if n != 2 {
		t.Fatalf("adopted = %d, want 2", n)
	}

	assertStatus := func(rowid int64, want string) {
		t.Helper()
		var got string
		switch err := db.QueryRow(`SELECT status FROM voice_index WHERE rowid = ?`, rowid).Scan(&got); err {
		case nil:
			if got != want {
				t.Errorf("rowid %d status = %q, want %q", rowid, got, want)
			}
		case sql.ErrNoRows:
			if want != "" {
				t.Errorf("rowid %d missing, want status %q", rowid, want)
			}
		default:
			t.Fatal(err)
		}
	}
	assertStatus(1, VoiceStatusDownloaded)  // adopted
	assertStatus(2, VoiceStatusTranscribed) // left untouched
	assertStatus(3, VoiceStatusDownloaded)  // adopted
	assertStatus(99, "")                    // not in DB → no row
	assertStatus(4, "")                     // image → no row

	// The adopted rows must now be queueable by the transcribe selector.
	cands, err := selectVoiceTranscribeCandidates(db, false, false, 0)
	if err != nil {
		t.Fatalf("select candidates: %v", err)
	}
	if got := rowidsOf(cands); len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Errorf("transcribe candidates = %v, want [1 3]", got)
	}

	// Idempotent: a second pass adopts nothing.
	if n, err := reconcileVoiceIndexFromDisk(db, voiceDir); err != nil || n != 0 {
		t.Fatalf("second reconcile = (%d, %v), want (0, nil)", n, err)
	}
}

// rowidsOf extracts sorted-by-query-order rowids from voice candidates.
func rowidsOf(cs []voiceCandidate) []int64 {
	out := make([]int64, len(cs))
	for i, c := range cs {
		out[i] = c.rowid
	}
	return out
}
