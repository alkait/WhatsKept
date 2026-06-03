package postprocess

import (
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

// seedCandidateDB builds an in-memory ChatStorage-shaped DB with seven
// image messages, each in a distinct media_index state, so the download
// and describe selectors can be asserted exhaustively:
//
//	1 — no media_index row              (never attempted)
//	2 — downloaded, describe_error NULL  (ready to describe)
//	3 — described, source='apple'        (apple base layer)
//	4 — described, source='cloud'        (already cloud)
//	5 — missing                          (not in backup)
//	6 — error                            (download failed, no file)
//	7 — downloaded, describe_error set    (prior describe failure)
func seedCandidateDB(t *testing.T) *sql.DB {
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
	exec(`CREATE TABLE ZWAMEDIAITEM (ZMESSAGE INTEGER, ZMEDIALOCALPATH TEXT)`)
	if err := ensureImageSidecarSchema(db); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 7; i++ {
		exec(`INSERT INTO ZWAMESSAGE (Z_PK, ZMESSAGETYPE) VALUES (?, 5)`, i)
		exec(`INSERT INTO ZWAMEDIAITEM (ZMESSAGE, ZMEDIALOCALPATH) VALUES (?, ?)`,
			i, fmt.Sprintf("Media/%d.jpg", i))
	}

	mi := func(rowid int64, status string, describeErr any) {
		exec(`INSERT INTO media_index (rowid, manifest_path, msg_type, status, bytes, error, describe_error, attempted_at)
		      VALUES (?, ?, 5, ?, 0, NULL, ?, '2020-01-01T00:00:00Z')`,
			rowid, fmt.Sprintf("Message/Media/%d.jpg", rowid), status, describeErr)
	}
	// rowid 1 deliberately has no media_index row.
	mi(2, MediaStatusDownloaded, nil)
	mi(3, MediaStatusDescribed, nil)
	mi(4, MediaStatusDescribed, nil)
	mi(5, MediaStatusMissing, nil)
	mi(6, MediaStatusError, nil)
	mi(7, MediaStatusDownloaded, "describe: boom")

	exec(`INSERT INTO wa_image_text (rowid, source, generated_at) VALUES (3, 'apple', '2020-01-01T00:00:00Z')`)
	exec(`INSERT INTO wa_image_text (rowid, source, generated_at) VALUES (4, 'cloud', '2020-01-01T00:00:00Z')`)
	return db
}

func rowids(cs []candidate) []int64 {
	out := make([]int64, len(cs))
	for i, c := range cs {
		out[i] = c.rowid
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

func TestSelectDownloadCandidates(t *testing.T) {
	db := seedCandidateDB(t)
	cases := []struct {
		name                      string
		retryMissing, retryErrors bool
		want                      []int64
	}{
		// Only the never-attempted row 1 is queued by default — rows 2/3
		// (on disk) are skipped, and 5/6 stay skipped without their flag.
		{"default", false, false, []int64{1}},
		{"retry_missing", true, false, []int64{1, 5}},
		{"retry_errors", false, true, []int64{1, 6}},
		{"retry_both", true, true, []int64{1, 5, 6}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := selectDownloadCandidates(db, c.retryMissing, c.retryErrors, 0)
			if err != nil {
				t.Fatal(err)
			}
			if g := rowids(got); !reflect.DeepEqual(g, c.want) {
				t.Errorf("download candidates = %v, want %v", g, c.want)
			}
		})
	}
}

func TestSelectDescribeCandidates(t *testing.T) {
	db := seedCandidateDB(t)
	cases := []struct {
		name        string
		engine      string
		retryErrors bool
		force       bool
		want        []int64
	}{
		// Apple, normal: only the clean downloaded row 2. Row 7 carries a
		// describe_error (skipped) and row 3 is already apple-described.
		{"apple_default", SourceApple, false, false, []int64{2}},
		{"apple_retry_errors", SourceApple, true, false, []int64{2, 7}},
		// Force re-runs apple over downloaded rows + apple-described rows.
		{"apple_force", SourceApple, false, true, []int64{2, 3, 7}},
		// Cloud upgrades: downloaded rows + any non-cloud described row
		// (row 3 apple), never the already-cloud row 4.
		{"cloud_default", SourceCloud, false, false, []int64{2, 3}},
		{"cloud_retry_errors", SourceCloud, true, false, []int64{2, 3, 7}},
		// Force re-describes every on-disk row regardless of source.
		{"cloud_force", SourceCloud, false, true, []int64{2, 3, 4, 7}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := selectDescribeCandidates(db, c.engine, c.retryErrors, c.force, 0)
			if err != nil {
				t.Fatal(err)
			}
			if g := rowids(got); !reflect.DeepEqual(g, c.want) {
				t.Errorf("describe candidates = %v, want %v", g, c.want)
			}
		})
	}
}

func TestCountDownloaded(t *testing.T) {
	db := seedCandidateDB(t)
	// Rows 2,3,4,7 are on disk (downloaded or described).
	got, err := countDownloaded(db)
	if err != nil {
		t.Fatal(err)
	}
	if got != 4 {
		t.Errorf("countDownloaded = %d, want 4", got)
	}
}
