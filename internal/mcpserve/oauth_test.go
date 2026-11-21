package mcpserve

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

const testToken = "0123456789abcdef0123456789abcdef"

// harness serves the full handler with a clock the test moves.
type harness struct {
	t     *testing.T
	ts    *httptest.Server
	clock atomic.Int64 // unix seconds
	http  *http.Client // never follows redirects
}

func newHarness(t *testing.T, token string) *harness {
	h := &harness{t: t, http: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	h.clock.Store(time.Now().Unix())
	h.ts = httptest.NewServer(newHandler(newMCPServer(writeFixtureDB(t)), token,
		func() time.Time { return time.Unix(h.clock.Load(), 0) }))
	t.Cleanup(h.ts.Close)
	return h
}

func (h *harness) advance(d time.Duration) { h.clock.Add(int64(d.Seconds())) }

func (h *harness) do(req *http.Request) (*http.Response, string) {
	h.t.Helper()
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func (h *harness) getJSON(path string) map[string]any {
	h.t.Helper()
	resp, body := h.do(mustGet(h.ts.URL + path))
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	return out
}

func (h *harness) post(path string, form url.Values) (*http.Response, string) {
	req, _ := http.NewRequest("POST", h.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return h.do(req)
}

func (h *harness) register(redirects ...string) (int, map[string]any) {
	h.t.Helper()
	b, _ := json.Marshal(map[string]any{"client_name": "Test App", "redirect_uris": redirects})
	req, _ := http.NewRequest("POST", h.ts.URL+"/register", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, body := h.do(req)
	var out map[string]any
	json.Unmarshal([]byte(body), &out)
	return resp.StatusCode, out
}

func (h *harness) clientID(redirect string) string {
	h.t.Helper()
	code, out := h.register(redirect)
	if code != http.StatusCreated {
		h.t.Fatalf("register: %d %v", code, out)
	}
	return out["client_id"].(string)
}

const (
	testRedirect = "http://127.0.0.1:9999/callback"
	testVerifier = "the-verifier-the-verifier-the-verifier-0123"
)

// authForm is a well-formed authorization request.
func authForm(clientID string) url.Values {
	return url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {testRedirect},
		"response_type":         {"code"},
		"state":                 {"st4te"},
		"code_challenge":        {pkce(testVerifier)},
		"code_challenge_method": {"S256"},
	}
}

// approve submits the consent form with the right token and returns
// the code from the redirect.
func (h *harness) approve(clientID string) string {
	h.t.Helper()
	form := authForm(clientID)
	form.Set("token", testToken)
	form.Set("action", "allow")
	resp, body := h.post("/authorize", form)
	if resp.StatusCode != http.StatusFound {
		h.t.Fatalf("approve: %d %s", resp.StatusCode, body)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("state") != "st4te" || loc.Query().Get("code") == "" {
		h.t.Fatalf("approve redirect = %s", loc)
	}
	return loc.Query().Get("code")
}

func (h *harness) exchange(form url.Values) (int, map[string]any) {
	h.t.Helper()
	resp, body := h.post("/token", form)
	var out map[string]any
	json.Unmarshal([]byte(body), &out)
	return resp.StatusCode, out
}

func codeGrant(clientID, code string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"redirect_uri":  {testRedirect},
		"code_verifier": {testVerifier},
	}
}

func (h *harness) tokens(clientID string) (access, refresh string) {
	h.t.Helper()
	status, out := h.exchange(codeGrant(clientID, h.approve(clientID)))
	if status != http.StatusOK {
		h.t.Fatalf("exchange: %d %v", status, out)
	}
	return out["access_token"].(string), out["refresh_token"].(string)
}

// mcpStatus is the status of an MCP initialize carrying auth.
func (h *harness) mcpStatus(auth string) int {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.ts.URL+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, _ := h.do(req)
	return resp.StatusCode
}

// TestOAuthDiscovery: every 401 points at the metadata, and every URL
// in it is built from the address the client used.
func TestOAuthDiscovery(t *testing.T) {
	h := newHarness(t, testToken)

	for _, auth := range []string{"", "Bearer wrong"} {
		req, _ := http.NewRequest("POST", h.ts.URL+"/mcp", strings.NewReader("{}"))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, _ := h.do(req)
		want := `Bearer resource_metadata="` + h.ts.URL + `/.well-known/oauth-protected-resource/mcp"`
		if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != want {
			t.Errorf("auth %q: %d %q, want 401 %q", auth, resp.StatusCode, resp.Header.Get("WWW-Authenticate"), want)
		}
	}
	if got := h.mcpStatus("Bearer " + testToken); got != http.StatusOK {
		t.Errorf("static token: status %d, want 200", got)
	}

	prm := h.getJSON("/.well-known/oauth-protected-resource/mcp")
	if prm["resource"] != h.ts.URL+"/mcp" || prm["authorization_servers"].([]any)[0] != h.ts.URL {
		t.Errorf("resource metadata = %v", prm)
	}
	if root := h.getJSON("/.well-known/oauth-protected-resource"); root["resource"] != h.ts.URL {
		t.Errorf("root resource metadata = %v", root)
	}
	asm := h.getJSON("/.well-known/oauth-authorization-server")
	for k, want := range map[string]string{
		"issuer":                 h.ts.URL,
		"authorization_endpoint": h.ts.URL + "/authorize",
		"token_endpoint":         h.ts.URL + "/token",
		"registration_endpoint":  h.ts.URL + "/register",
	} {
		if asm[k] != want {
			t.Errorf("%s = %v, want %s", k, asm[k], want)
		}
	}
	if m := asm["code_challenge_methods_supported"].([]any); len(m) != 1 || m[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v", m)
	}

	// Behind a tunnel: public Host, TLS terminated upstream.
	req, _ := http.NewRequest("GET", h.ts.URL+"/.well-known/oauth-authorization-server", nil)
	req.Host = "kept.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	_, body := h.do(req)
	if !strings.Contains(body, `"issuer":"https://kept.example.com"`) ||
		!strings.Contains(body, `"token_endpoint":"https://kept.example.com/token"`) {
		t.Errorf("proxied metadata = %s", body)
	}

	// Browser clients: preflight answered, CORS open.
	req, _ = http.NewRequest("OPTIONS", h.ts.URL+"/register", nil)
	resp, _ := h.do(req)
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestOAuthRegister(t *testing.T) {
	h := newHarness(t, testToken)
	status, out := h.register(testRedirect, "https://claude.ai/api/mcp/auth_callback", "cursor://anysphere/callback")
	if status != http.StatusCreated || out["token_endpoint_auth_method"] != "none" || out["client_name"] != "Test App" {
		t.Fatalf("register: %d %v", status, out)
	}
	if _, ok := newOAuth(testToken, time.Now).verify(out["client_id"].(string), kindClient); !ok {
		t.Error("client_id does not verify")
	}
	for _, bad := range [][]string{
		nil,
		{"http://evil.example/cb"},
		{"javascript:alert(1)"},
		{"https://ok.example/cb#frag"},
		{"/relative"},
	} {
		if status, out := h.register(bad...); status != http.StatusBadRequest || out["error"] != "invalid_redirect_uri" {
			t.Errorf("redirects %v: %d %v, want 400 invalid_redirect_uri", bad, status, out)
		}
	}
	req, _ := http.NewRequest("POST", h.ts.URL+"/register", strings.NewReader("not json"))
	if resp, _ := h.do(req); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("non-JSON body: %d, want 400", resp.StatusCode)
	}
}

// TestOAuthSDKClient: the SDK's own OAuth client discovers, registers,
// is approved, and talks MCP — the conformance check.
func TestOAuthSDKClient(t *testing.T) {
	h := newHarness(t, testToken)
	var sawPage bool
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:   "SDK Client",
				RedirectURIs: []string{testRedirect},
				GrantTypes:   []string{"authorization_code", "refresh_token"},
			},
		},
		RedirectURL: testRedirect,
		AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			// Play the browser: open the page, paste the token, allow.
			req, _ := http.NewRequest("GET", args.URL, nil)
			resp, body := h.do(req)
			sawPage = resp.StatusCode == 200 && strings.Contains(body, "SDK Client") &&
				strings.Contains(body, "http://127.0.0.1:9999")
			form := url.Values{"token": {testToken}, "action": {"allow"}}
			u, _ := url.Parse(args.URL)
			for k, v := range u.Query() {
				form[k] = v
			}
			resp, _ = h.post("/authorize", form)
			loc, err := url.Parse(resp.Header.Get("Location"))
			if err != nil {
				return nil, err
			}
			return &auth.AuthorizationResult{Code: loc.Query().Get("code"), State: loc.Query().Get("state")}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: h.ts.URL + "/mcp", OAuthHandler: handler}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if !sawPage {
		t.Error("consent page did not name the client and its return address")
	}
	var out searchOut
	call(t, sess, "search", map[string]any{"query": "café"}, &out)
	if len(out.Hits) != 1 {
		t.Errorf("hits over OAuth = %+v", out.Hits)
	}
}

func TestOAuthConsentPage(t *testing.T) {
	h := newHarness(t, testToken)
	id := h.clientID(testRedirect)

	resp, body := h.do(mustGet(h.ts.URL + "/authorize?" + authForm(id).Encode()))
	if resp.StatusCode != 200 || !strings.Contains(body, "Test App") || !strings.Contains(body, "send messages as you") {
		t.Errorf("consent page: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("consent page can be framed: %v", resp.Header)
	}

	// Wrong token: the page again, no redirect.
	form := authForm(id)
	form.Set("token", "wrong")
	resp, body = h.post("/authorize", form)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "not right") {
		t.Errorf("wrong token: %d %s", resp.StatusCode, body)
	}

	// Deny: back to the app with access_denied.
	form = authForm(id)
	form.Set("action", "deny")
	resp, _ = h.post("/authorize", form)
	if loc := resp.Header.Get("Location"); resp.StatusCode != 302 ||
		!strings.HasPrefix(loc, testRedirect+"?") || !strings.Contains(loc, "error=access_denied") || !strings.Contains(loc, "state=st4te") {
		t.Errorf("deny: %d %q", resp.StatusCode, loc)
	}

	// No PKCE: back to the app with invalid_request.
	form = authForm(id)
	form.Del("code_challenge")
	resp, _ = h.do(mustGet(h.ts.URL + "/authorize?" + form.Encode()))
	if loc := resp.Header.Get("Location"); resp.StatusCode != 302 || !strings.Contains(loc, "error=invalid_request") {
		t.Errorf("no PKCE: %d %q", resp.StatusCode, loc)
	}

	// Unknown client or unregistered redirect: an error page, never a
	// redirect to an address we can't vouch for.
	for name, form := range map[string]url.Values{
		"tampered client": authForm(id + "x"),
		"other redirect": func() url.Values {
			f := authForm(id)
			f.Set("redirect_uri", "http://127.0.0.1:1/elsewhere")
			return f
		}(),
	} {
		resp, _ := h.do(mustGet(h.ts.URL + "/authorize?" + form.Encode()))
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
			t.Errorf("%s: %d %q", name, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

func mustGet(u string) *http.Request {
	req, _ := http.NewRequest("GET", u, nil)
	return req
}

func TestOAuthCodeExchange(t *testing.T) {
	h := newHarness(t, testToken)
	id := h.clientID(testRedirect)
	other := h.clientID(testRedirect)

	cases := []struct {
		name  string
		form  func(code string) url.Values
		after time.Duration
	}{
		{"wrong verifier", func(c string) url.Values { f := codeGrant(id, c); f.Set("code_verifier", "nope"); return f }, 0},
		{"other client", func(c string) url.Values { return codeGrant(other, c) }, 0},
		{"no client", func(c string) url.Values { f := codeGrant(id, c); f.Del("client_id"); return f }, 0},
		{"other redirect", func(c string) url.Values {
			f := codeGrant(id, c)
			f.Set("redirect_uri", "http://127.0.0.1/x")
			return f
		}, 0},
		{"expired", func(c string) url.Values { return codeGrant(id, c) }, codeTTL},
		{"garbage", func(string) url.Values { return codeGrant(id, "garbage") }, 0},
	}
	for _, tc := range cases {
		code := h.approve(id)
		h.advance(tc.after)
		if status, out := h.exchange(tc.form(code)); status != 400 || out["error"] != "invalid_grant" {
			t.Errorf("%s: %d %v, want 400 invalid_grant", tc.name, status, out)
		}
	}

	code := h.approve(id)
	status, out := h.exchange(codeGrant(id, code))
	if status != 200 || out["token_type"] != "Bearer" || out["expires_in"] != float64(3600) || out["refresh_token"] == nil {
		t.Fatalf("exchange: %d %v", status, out)
	}
	if got := h.mcpStatus("Bearer " + out["access_token"].(string)); got != 200 {
		t.Errorf("access token on /mcp: %d, want 200", got)
	}
	if status, out := h.exchange(codeGrant(id, code)); status != 400 || out["error"] != "invalid_grant" {
		t.Errorf("reused code: %d %v, want invalid_grant", status, out)
	}
	if status, out := h.exchange(url.Values{"grant_type": {"password"}}); status != 400 || out["error"] != "unsupported_grant_type" {
		t.Errorf("password grant: %d %v", status, out)
	}

	// A blob of one kind never passes for another.
	for _, wrong := range []string{id, code, out["refresh_token"].(string)} {
		if got := h.mcpStatus("Bearer " + wrong); got != 401 {
			t.Errorf("non-access blob on /mcp: %d, want 401", got)
		}
	}
}

func TestOAuthRefresh(t *testing.T) {
	h := newHarness(t, testToken)
	id := h.clientID(testRedirect)
	access, refresh := h.tokens(id)

	// The access token lapses after an hour; the refresh token renews it.
	h.advance(accessTTL)
	if got := h.mcpStatus("Bearer " + access); got != 401 {
		t.Errorf("expired access token: %d, want 401", got)
	}
	status, out := h.exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {id}})
	if status != 200 {
		t.Fatalf("refresh: %d %v", status, out)
	}
	if got := h.mcpStatus("Bearer " + out["access_token"].(string)); got != 200 {
		t.Errorf("refreshed access token: %d, want 200", got)
	}
	if out["refresh_token"] == refresh {
		t.Error("refresh must issue a new refresh token")
	}
	if status, _ := h.exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh},
		"client_id": {h.clientID(testRedirect)}}); status != 400 {
		t.Errorf("refresh by another client: %d, want 400", status)
	}

	// Through the oauth2 library too, as real clients do it.
	cfg := &oauth2.Config{ClientID: id, Endpoint: oauth2.Endpoint{TokenURL: h.ts.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}}
	tok, err := cfg.TokenSource(context.Background(), &oauth2.Token{RefreshToken: out["refresh_token"].(string), Expiry: time.Unix(1, 0)}).Token()
	if err != nil || tok.AccessToken == "" {
		t.Errorf("oauth2 refresh: %v", err)
	}

	// After 90 days the refresh token is dead too.
	h.advance(refreshTTL)
	if status, out := h.exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {out["refresh_token"].(string)}}); status != 400 || out["error"] != "invalid_grant" {
		t.Errorf("expired refresh: %d %v", status, out)
	}
}

// TestOAuthRotation: a new WHATSKEPT_MCP_TOKEN signs every OAuth client
// out — the server restarted with it no longer honours old tokens.
func TestOAuthRotation(t *testing.T) {
	h := newHarness(t, testToken)
	id := h.clientID(testRedirect)
	access, refresh := h.tokens(id)

	rotated := newHarness(t, strings.Repeat("f", 32))
	if got := rotated.mcpStatus("Bearer " + access); got != 401 {
		t.Errorf("old access token after rotation: %d, want 401", got)
	}
	if status, _ := rotated.exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}); status != 400 {
		t.Errorf("old refresh token after rotation: %d, want 400", status)
	}
	if resp, _ := rotated.do(mustGet(rotated.ts.URL + "/authorize?" + authForm(id).Encode())); resp.StatusCode != 400 {
		t.Errorf("old client_id after rotation: %d, want 400", resp.StatusCode)
	}
	if got := rotated.mcpStatus("Bearer " + strings.Repeat("f", 32)); got != 200 {
		t.Errorf("new static token: %d, want 200", got)
	}
}
