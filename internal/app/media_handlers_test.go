package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// post is a tiny helper for the handler tests below.
func post(mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

// startedJobID asserts the response started a job and returns its id.
func startedJobID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (job started), got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.JobID == "" {
		t.Fatalf("expected a job_id, got %s (err %v)", rec.Body.String(), err)
	}
	return resp.JobID
}

// waitForJob blocks until the in-process job's goroutine has returned
// (its "done" event is emitted only after work() returns). Without this,
// the job keeps writing into the test's t.TempDir() while cleanup runs,
// racing into a "directory not empty" failure.
func waitForJob(t *testing.T, s *server, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, ok := s.jobs.get(id)
		if !ok {
			return
		}
		if _, finished := j.snapshot(); finished {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not finish within 5s")
}

// TestMediaIndexStartsWithoutPassword: the Apple Vision scan reads images
// off disk, so it must NOT require the backup password — a workspace
// alone is enough to start the job.
func TestMediaIndexStartsWithoutPassword(t *testing.T) {
	// No workspace → 400.
	_, mux := testServer()
	if rec := post(mux, "/api/database/media-index", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("no workspace: code = %d", rec.Code)
	}

	// Workspace open, no password supplied or cached → should still start.
	s, mux2 := testServer()
	s.ws.set(t.TempDir())
	id := startedJobID(t, post(mux2, "/api/database/media-index", `{}`))
	waitForJob(t, s, id)
}

// TestMediaDescribeStartsWithoutPassword: with an API key present, cloud
// describe also reads off disk and needs no backup password.
func TestMediaDescribeStartsWithoutPassword(t *testing.T) {
	s, mux := testServer()
	s.ws.set(t.TempDir())
	s.apiKey.set("sk-or-test")
	id := startedJobID(t, post(mux, "/api/database/media-describe", `{}`))
	waitForJob(t, s, id)
}
