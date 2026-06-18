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

// TestMediaDescribeStartsWithoutPassword: with an API key present, cloud
// describe reads images off disk and needs no backup password.
func TestMediaDescribeStartsWithoutPassword(t *testing.T) {
	s, mux := testServer()
	s.ws.set(t.TempDir())
	s.apiKey.set("sk-or-test")
	id := startedJobID(t, post(mux, "/api/database/media-describe", `{}`))
	waitForJob(t, s, id)
}
