package mcpserve

// OAuth 2.1 for the HTTP transport, beside the static bearer token.
//
// whatskept is its own authorization server, and a stateless one:
// client IDs, codes and tokens are HMAC-signed blobs keyed off
// WHATSKEPT_MCP_TOKEN, so nothing is stored, tokens survive restarts,
// and rotating the token signs every OAuth client out. A client is
// approved by pasting that same token on the consent page.
//
// Every public URL is built from the request (Host, X-Forwarded-Proto):
// how the server is exposed is the deployment's business, not ours.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	codeTTL    = time.Minute
	accessTTL  = time.Hour
	refreshTTL = 90 * 24 * time.Hour
)

type oauth struct {
	static []byte // the bearer token — also the consent password
	key    []byte // HMAC key derived from it
	now    func() time.Time

	mu   sync.Mutex
	used map[string]int64 // spent code nonces → their expiry
}

func newOAuth(token string, now func() time.Time) *oauth {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("whatskept-oauth-v1"))
	return &oauth{static: []byte(token), key: m.Sum(nil), now: now, used: map[string]int64{}}
}

// claims is the payload of every signed blob. Kind is signed with the
// rest, so a blob of one kind never passes for another.
type claims struct {
	Kind      string   `json:"k"`
	Exp       int64    `json:"e,omitempty"`
	Nonce     string   `json:"j"`
	Name      string   `json:"n,omitempty"` // client: display name
	Redirects []string `json:"r,omitempty"` // client: registered redirect URIs
	Client    string   `json:"c,omitempty"` // code, access, refresh: client fingerprint
	Redirect  string   `json:"u,omitempty"` // code: the redirect it was issued for
	Challenge string   `json:"p,omitempty"` // code: PKCE S256 challenge
}

const (
	kindClient  = "client"
	kindCode    = "code"
	kindAccess  = "access"
	kindRefresh = "refresh"
)

var b64 = base64.RawURLEncoding

func (o *oauth) sign(c claims) string {
	c.Nonce = rand.Text()
	body, _ := json.Marshal(c)
	p := b64.EncodeToString(body)
	return p + "." + o.mac(p)
}

func (o *oauth) mac(p string) string {
	m := hmac.New(sha256.New, o.key)
	m.Write([]byte(p))
	return b64.EncodeToString(m.Sum(nil))
}

// verify returns the claims of a blob signed by this server, of the
// given kind and not expired.
func (o *oauth) verify(s, kind string) (claims, bool) {
	p, sig, ok := strings.Cut(s, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(o.mac(p))) {
		return claims{}, false
	}
	body, err := b64.DecodeString(p)
	var c claims
	if err != nil || json.Unmarshal(body, &c) != nil || c.Kind != kind {
		return claims{}, false
	}
	if c.Exp != 0 && o.now().Unix() >= c.Exp {
		return claims{}, false
	}
	return c, true
}

// fingerprint ties codes and tokens to a client without carrying its
// whole (long) client_id.
func fingerprint(clientID string) string {
	sum := sha256.Sum256([]byte(clientID))
	return b64.EncodeToString(sum[:12])
}

// spend marks a code as used; false if it already was.
func (o *oauth) spend(nonce string, exp int64) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now().Unix()
	for n, e := range o.used {
		if e <= now {
			delete(o.used, n)
		}
	}
	if _, dup := o.used[nonce]; dup {
		return false
	}
	o.used[nonce] = exp
	return true
}

// baseURL is the origin the client used to reach us. A proxy that
// terminates TLS says so with X-Forwarded-Proto.
func baseURL(r *http.Request) string {
	scheme := "http"
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	if r.TLS != nil || strings.EqualFold(strings.TrimSpace(proto), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// routes serves /mcp behind the gate, plus the OAuth endpoints.
func (o *oauth) routes(mcpHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", o.gate(mcpHandler))
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", public(http.MethodGet, o.resourceMeta("/mcp")))
	// The root form describes the origin itself (RFC 9728 §3.1).
	mux.HandleFunc("/.well-known/oauth-protected-resource", public(http.MethodGet, o.resourceMeta("")))
	mux.HandleFunc("/.well-known/oauth-authorization-server", public(http.MethodGet, o.serverMeta))
	mux.HandleFunc("/register", public(http.MethodPost, o.register))
	mux.HandleFunc("GET /authorize", o.authorize)
	mux.HandleFunc("POST /authorize", o.authorize)
	mux.HandleFunc("/token", public(http.MethodPost, o.token))
	return mux
}

// gate admits the static token or a valid OAuth access token. The
// "Bearer " prefix is optional: some clients send the configured header
// value verbatim. Every 401 points at the OAuth metadata, as the MCP
// spec requires.
func (o *oauth) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if len(got) > 7 && strings.EqualFold(got[:7], "Bearer ") {
			got = got[7:]
		}
		if subtle.ConstantTimeCompare([]byte(got), o.static) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := o.verify(got, kindAccess); ok {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp"`, baseURL(r)))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// public wraps an endpoint clients may call from a browser: CORS open,
// preflight answered, one method allowed (GET also admits HEAD).
func public(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", method+", OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != method && !(method == http.MethodGet && r.Method == http.MethodHead) {
			w.Header().Set("Allow", method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func (o *oauth) resourceMeta(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		base := baseURL(r)
		writeJSON(w, http.StatusOK, map[string]any{
			"resource":                 base + path,
			"authorization_servers":    []string{base},
			"bearer_methods_supported": []string{"header"},
			"resource_name":            "WhatsKept",
		})
	}
}

func (o *oauth) serverMeta(w http.ResponseWriter, r *http.Request) {
	base := baseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/authorize",
		"token_endpoint":                        base + "/token",
		"registration_endpoint":                 base + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

// register is dynamic client registration (RFC 7591), open to anyone:
// a client can do nothing until the owner approves it. Every client is
// public (no secret); its client_id carries its signed metadata.
func (o *oauth) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body must be JSON client metadata")
		return
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "register between 1 and 10 redirect_uris")
		return
	}
	for _, u := range in.RedirectURIs {
		if !validRedirect(u) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri",
				"redirect_uris must be https, http on loopback, or an app scheme, without a fragment: "+u)
			return
		}
	}
	name := in.ClientName
	if len(name) > 100 {
		name = name[:100]
	}
	out := map[string]any{
		"client_id":                  o.sign(claims{Kind: kindClient, Name: name, Redirects: in.RedirectURIs}),
		"client_id_issued_at":        o.now().Unix(),
		"redirect_uris":              in.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	if name != "" {
		out["client_name"] = name
	}
	writeJSON(w, http.StatusCreated, out)
}

func validRedirect(s string) bool {
	u, err := url.Parse(s)
	if err != nil || !u.IsAbs() || u.Fragment != "" || len(s) > 2000 {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return u.Host != ""
	case "http":
		h := u.Hostname()
		ip := net.ParseIP(h)
		return h == "localhost" || (ip != nil && ip.IsLoopback())
	case "javascript", "data", "vbscript", "file", "blob", "about":
		return false
	}
	return true // a native app's own scheme
}

// authorize shows the consent page (GET) and acts on it (POST). Until
// the client and its redirect are known good, errors render here
// rather than redirecting anywhere.
func (o *oauth) authorize(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		renderConsent(w, http.StatusBadRequest, consentView{Error: "This request could not be read."})
		return
	}
	q := r.Form
	clientID := q.Get("client_id")
	client, ok := o.verify(clientID, kindClient)
	if !ok {
		renderConsent(w, http.StatusBadRequest, consentView{Error: "This app is not registered here. Remove the connector and add it again."})
		return
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" && len(client.Redirects) == 1 {
		redirect = client.Redirects[0]
	}
	if !slices.Contains(client.Redirects, redirect) {
		renderConsent(w, http.StatusBadRequest, consentView{Error: "This app asked to return to an address it did not register."})
		return
	}
	back := func(params url.Values) {
		u, _ := url.Parse(redirect)
		v := u.Query()
		for k, vs := range params {
			v[k] = vs
		}
		if st := q.Get("state"); st != "" {
			v.Set("state", st)
		}
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	}
	if q.Get("response_type") != "code" {
		back(url.Values{"error": {"unsupported_response_type"}, "error_description": {"only response_type=code is supported"}})
		return
	}
	challenge := q.Get("code_challenge")
	if challenge == "" || q.Get("code_challenge_method") != "S256" {
		back(url.Values{"error": {"invalid_request"}, "error_description": {"PKCE with S256 is required"}})
		return
	}

	ru, _ := url.Parse(redirect)
	view := consentView{Client: client.Name, Return: ru.Scheme + "://" + ru.Host}
	if view.Client == "" {
		view.Client = "An unnamed app"
	}
	for _, k := range []string{"client_id", "redirect_uri", "response_type", "state", "code_challenge", "code_challenge_method"} {
		view.Fields = append(view.Fields, field{k, q.Get(k)})
	}
	if r.Method == http.MethodGet {
		renderConsent(w, http.StatusOK, view)
		return
	}
	if q.Get("action") == "deny" {
		back(url.Values{"error": {"access_denied"}})
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(q.Get("token"))), o.static) != 1 {
		view.Wrong = true
		renderConsent(w, http.StatusForbidden, view)
		return
	}
	code := o.sign(claims{
		Kind:      kindCode,
		Exp:       o.now().Add(codeTTL).Unix(),
		Client:    fingerprint(clientID),
		Redirect:  redirect,
		Challenge: challenge,
	})
	back(url.Values{"code": {code}})
}

// token exchanges a code (with its PKCE verifier) or a refresh token
// for a fresh access + refresh token pair.
func (o *oauth) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body must be form-encoded")
		return
	}
	f := r.PostForm
	clientID := f.Get("client_id")
	if user, _, ok := r.BasicAuth(); ok && clientID == "" {
		clientID, _ = url.QueryUnescape(user)
	}
	switch f.Get("grant_type") {
	case "authorization_code":
		code, ok := o.verify(f.Get("code"), kindCode)
		switch {
		case !ok:
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is invalid or expired")
		case fingerprint(clientID) != code.Client:
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued to another client")
		case f.Get("redirect_uri") != "" && f.Get("redirect_uri") != code.Redirect:
			oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		case pkce(f.Get("code_verifier")) != code.Challenge:
			oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match the code_challenge")
		case !o.spend(code.Nonce, code.Exp):
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was already used")
		default:
			o.issue(w, code.Client)
		}
	case "refresh_token":
		rt, ok := o.verify(f.Get("refresh_token"), kindRefresh)
		switch {
		case !ok:
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid or expired")
		case clientID != "" && fingerprint(clientID) != rt.Client:
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was issued to another client")
		default:
			o.issue(w, rt.Client)
		}
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

func pkce(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return b64.EncodeToString(sum[:])
}

func (o *oauth) issue(w http.ResponseWriter, client string) {
	now := o.now()
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  o.sign(claims{Kind: kindAccess, Exp: now.Add(accessTTL).Unix(), Client: client}),
		"token_type":    "Bearer",
		"expires_in":    int(accessTTL.Seconds()),
		"refresh_token": o.sign(claims{Kind: kindRefresh, Exp: now.Add(refreshTTL).Unix(), Client: client}),
	})
}

type field struct{ Name, Value string }

type consentView struct {
	Error  string
	Client string
	Return string
	Fields []field
	Wrong  bool
}

func renderConsent(w http.ResponseWriter, status int, v consentView) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	consentPage.Execute(w, v)
}

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect to WhatsKept</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;max-width:28rem;margin:4rem auto;padding:0 1rem;color:#111}
input{width:100%;box-sizing:border-box;padding:.5rem;font:inherit;margin:.25rem 0 1rem}
button{padding:.5rem 1rem;font:inherit}.err{color:#b00020}
</style></head><body>
<h1>Connect to WhatsKept</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{else}}
<p><strong>{{.Client}}</strong> wants access. It will be able to read your entire WhatsApp history and send messages as you.</p>
<p>Allowing it returns you to <strong>{{.Return}}</strong>. Continue only if you started this from that app.</p>
<form method="post">
{{range .Fields}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">
{{end}}<label for="token">Your WHATSKEPT_MCP_TOKEN</label>
<input id="token" name="token" type="password" autocomplete="off" required autofocus>
{{if .Wrong}}<p class="err">That token is not right.</p>{{end}}
<button name="action" value="allow">Allow</button>
<button name="action" value="deny" formnovalidate>Deny</button>
</form>{{end}}
</body></html>
`))
