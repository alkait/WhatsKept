package postprocess

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func TestSplitTextDescription(t *testing.T) {
	cases := []struct {
		name, in, wantOCR, wantDesc string
	}{
		{
			name:     "both sections",
			in:       "TEXT: Invoice #4471\nAED 12,500\nDESCRIPTION: A printed quotation.",
			wantOCR:  "Invoice #4471\nAED 12,500",
			wantDesc: "A printed quotation.",
		},
		{
			name:     "no text marker, still has description",
			in:       "DESCRIPTION: A gold necklace in a box.",
			wantOCR:  "",
			wantDesc: "A gold necklace in a box.",
		},
		{
			name:     "none normalises to empty ocr",
			in:       "TEXT: none\nDESCRIPTION: A sunset over the sea.",
			wantOCR:  "",
			wantDesc: "A sunset over the sea.",
		},
		{
			name:     "markers absent → all description",
			in:       "A cat sitting on a mat.",
			wantOCR:  "",
			wantDesc: "A cat sitting on a mat.",
		},
		{
			name:     "case-insensitive labels",
			in:       "text: hello\ndescription: a greeting.",
			wantOCR:  "hello",
			wantDesc: "a greeting.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ocr, desc := splitTextDescription(tc.in)
			if ocr != tc.wantOCR {
				t.Errorf("ocr = %q, want %q", ocr, tc.wantOCR)
			}
			if desc != tc.wantDesc {
				t.Errorf("desc = %q, want %q", desc, tc.wantDesc)
			}
		})
	}
}

// TestMigrateImageSidecar verifies an old (pre-provenance) wa_image_text
// is upgraded in place: the new columns appear, existing rows default to
// source='apple' (which is historically correct), and the migration is
// idempotent.
func TestMigrateImageSidecar(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// The original v1 schema (6 columns, no description/source/model).
	if _, err := db.Exec(`CREATE TABLE wa_image_text (
		rowid INTEGER PRIMARY KEY,
		ocr_text TEXT NOT NULL DEFAULT '',
		language TEXT NOT NULL DEFAULT '',
		labels TEXT NOT NULL DEFAULT '',
		label_scores TEXT,
		generated_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO wa_image_text (rowid, ocr_text, generated_at) VALUES (1, 'hello', '2020-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ { // run twice: must be idempotent
		if err := migrateImageSidecar(db); err != nil {
			t.Fatalf("migrate (pass %d): %v", i, err)
		}
	}

	cols, err := tableColumns(db, "main", "wa_image_text")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"description", "source", "model"} {
		if !cols[want] {
			t.Errorf("missing column %q after migration", want)
		}
	}

	var src, desc, model string
	if err := db.QueryRow(
		`SELECT source, description, model FROM wa_image_text WHERE rowid = 1`,
	).Scan(&src, &desc, &model); err != nil {
		t.Fatal(err)
	}
	if src != SourceApple {
		t.Errorf("existing row source = %q, want %q", src, SourceApple)
	}
	if desc != "" || model != "" {
		t.Errorf("existing row description/model = %q/%q, want empty", desc, model)
	}
}

// TestMigrateMediaIndexDescribeError verifies an old media_index (created
// before the download/describe split, so without describe_error) gains
// the column idempotently, while leaving existing rows untouched.
func TestMigrateMediaIndexDescribeError(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Pre-split media_index (7 columns, no describe_error). wa_image_text
	// must exist too since migrateImageSidecar inspects both.
	if _, err := db.Exec(`
		CREATE TABLE wa_image_text (rowid INTEGER PRIMARY KEY, ocr_text TEXT NOT NULL DEFAULT '',
			language TEXT NOT NULL DEFAULT '', labels TEXT NOT NULL DEFAULT '', label_scores TEXT,
			description TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT 'apple',
			model TEXT NOT NULL DEFAULT '', generated_at TEXT NOT NULL);
		CREATE TABLE media_index (rowid INTEGER PRIMARY KEY, manifest_path TEXT NOT NULL,
			msg_type INTEGER NOT NULL, status TEXT NOT NULL, bytes INTEGER, error TEXT,
			attempted_at TEXT NOT NULL);
		INSERT INTO media_index (rowid, manifest_path, msg_type, status, attempted_at)
			VALUES (1, 'Message/Media/1.jpg', 5, 'described', '2020-01-01T00:00:00Z');`,
	); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ { // idempotent
		if err := migrateImageSidecar(db); err != nil {
			t.Fatalf("migrate (pass %d): %v", i, err)
		}
	}

	cols, err := tableColumns(db, "main", "media_index")
	if err != nil {
		t.Fatal(err)
	}
	if !cols["describe_error"] {
		t.Fatal("media_index missing describe_error column after migration")
	}
	// Existing row preserved, new column defaults to NULL.
	var status string
	var describeErr sql.NullString
	if err := db.QueryRow(`SELECT status, describe_error FROM media_index WHERE rowid = 1`).
		Scan(&status, &describeErr); err != nil {
		t.Fatal(err)
	}
	if status != MediaStatusDescribed {
		t.Errorf("status = %q, want %q", status, MediaStatusDescribed)
	}
	if describeErr.Valid {
		t.Errorf("describe_error = %q, want NULL", describeErr.String)
	}
}

// TestCloudDescribeLive exercises the real OpenRouter path end-to-end.
// Skipped unless OPENROUTER_API_KEY and WHATSKEPT_TEST_IMAGE (a path to
// a JPEG/PNG) are both set, so normal `go test` runs stay offline/free.
func TestCloudDescribeLive(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	imgPath := os.Getenv("WHATSKEPT_TEST_IMAGE")
	if key == "" || imgPath == "" {
		t.Skip("set OPENROUTER_API_KEY and WHATSKEPT_TEST_IMAGE to run the live cloud test")
	}
	data, err := os.ReadFile(imgPath)
	if err != nil {
		t.Fatalf("read test image: %v", err)
	}
	model := os.Getenv("WHATSKEPT_TEST_MODEL") // optional override
	d, err := newCloudDescriber(key, model)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := d.Describe(ctx, 1, imgPath, data)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if res.OCRText == "" && res.Description == "" {
		t.Fatal("both OCRText and Description empty — model returned nothing usable")
	}
	t.Logf("source=%s model=%s\n  OCR: %.120q\n  DESC: %.120q",
		d.Source(), d.Model(), res.OCRText, res.Description)
}
