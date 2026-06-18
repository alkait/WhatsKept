package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testServer builds a server with the same wiring as newServer but
// without binding a TCP port, and returns it with its route mux. It
// isolates credential persistence to a temp dir so tests never touch
// (or delete) a real user's ~/.config/whatskept/credentials.json.
func testServer(t *testing.T) (*server, *http.ServeMux) {
	t.Setenv("WHATSKEPT_CONFIG_DIR", t.TempDir())
	s := &server{
		ws:     newWorkspaceState(),
		jobs:   newJobManager(),
		pw:     newPasswordStore(),
		apiKey: newAPIKeyStore(),
	}
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	return s, mux
}

func TestSessionAPIKeyStatusAndClear(t *testing.T) {
	s, mux := testServer(t)

	// Initially absent.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/session/openrouter-key", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: code = %d", rec.Code)
	}
	var st struct {
		Has bool `json:"has"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Has {
		t.Error("expected has=false on a fresh session")
	}

	// Set it directly (bypassing the network-validating POST), then the
	// status endpoint should report it.
	s.apiKey.set("sk-or-test", false)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/session/openrouter-key", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if !st.Has {
		t.Error("expected has=true after set")
	}

	// DELETE clears it.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/session/openrouter-key", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("clear: code = %d", rec.Code)
	}
	if s.apiKey.has() {
		t.Error("expected key cleared after DELETE")
	}
}

func TestAPIKeyPersistenceRoundTrip(t *testing.T) {
	t.Setenv("WHATSKEPT_CONFIG_DIR", t.TempDir())

	// Persist a key, then simulate a restart by building a fresh store
	// over the same config dir — it should load the key.
	a := newAPIKeyStore()
	if err := a.set("sk-or-remembered", true); err != nil {
		t.Fatalf("set persist: %v", err)
	}
	if restarted := newAPIKeyStore(); restarted.get() != "sk-or-remembered" || !restarted.isPersisted() {
		t.Fatalf("after restart: get=%q persisted=%v; want remembered,true", restarted.get(), restarted.isPersisted())
	}

	// Forget wipes RAM + disk; a fresh store sees nothing.
	if err := a.clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if restarted := newAPIKeyStore(); restarted.has() {
		t.Fatal("expected no key after forget + restart")
	}

	// Session-only set must not survive a restart, and must remove any
	// previously persisted copy.
	a2 := newAPIKeyStore()
	if err := a2.set("sk-or-persisted", true); err != nil {
		t.Fatal(err)
	}
	if err := a2.set("sk-or-session", false); err != nil {
		t.Fatal(err)
	}
	if restarted := newAPIKeyStore(); restarted.has() {
		t.Fatalf("session-only key should not persist; got %q", restarted.get())
	}
}

func TestSessionAPIKeySetRejectsEmpty(t *testing.T) {
	_, mux := testServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/session/openrouter-key",
		strings.NewReader(`{"key":""}`))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty key, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestMediaDescribeStatusNoWorkspace(t *testing.T) {
	_, mux := testServer(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/database/media-describe/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var resp mediaDescribeStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.HasKey {
		t.Error("expected has_key=false")
	}
	if resp.DefaultModel == "" {
		t.Error("expected a default model to be reported")
	}
}

func TestCancelJob(t *testing.T) {
	s, mux := testServer(t)

	// Unknown id → 404.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/jobs/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job: code = %d", rec.Code)
	}

	// A cancellable job that blocks until its context is cancelled.
	done := make(chan struct{})
	id := s.jobs.startInProcessProgressCtx("test",
		func(ctx context.Context, _ func(string), _ func(any)) error {
			<-ctx.Done()
			close(done)
			return ctx.Err()
		})

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/jobs/"+id, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: code = %d (%s)", rec.Code, rec.Body.String())
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("job was not cancelled within 2s")
	}
}

func TestMediaDescribeRequiresKey(t *testing.T) {
	s, mux := testServer(t)
	// Pretend a workspace is open so we get past that check and hit the
	// key requirement. (Path need not exist for this 400 path.)
	s.ws.set("/tmp/nonexistent-workspace")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/database/media-describe",
		strings.NewReader(`{"password":"x"}`))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without a key, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "API key") {
		t.Errorf("expected an API-key error, got %s", rec.Body.String())
	}
}
