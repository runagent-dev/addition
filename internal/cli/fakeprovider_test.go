package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/runagent-dev/addition/internal/vault"
	"github.com/runagent-dev/addition/shared"
)

const (
	fakeClientID     = "1234-fake.apps.googleusercontent.com"
	fakeClientSecret = "GOCSPX-fake-client-secret-0001"
	fakeAccessToken  = "ya29.fake-access-token-0001"
	fakeRefreshToken = "1//fake-refresh-token-0001"
	workIdentity     = "me@work.example"
	workSubject      = "1001"
	// echoMarker tags provider diagnostics that repeat what addn sent; it must never be shown.
	echoMarker = "echo-marker"
)

const (
	scopeOpenID   = "openid"
	scopeEmail    = "https://www.googleapis.com/auth/userinfo.email"
	scopeProfile  = "https://www.googleapis.com/auth/userinfo.profile"
	scopeGmailRO  = "https://www.googleapis.com/auth/gmail.readonly"
	scopeDrive    = "https://www.googleapis.com/auth/drive"
	scopeCalendar = "https://www.googleapis.com/auth/calendar"
)

type issuedCode struct {
	challenge   string
	redirectURI string
	scopes      []string
}

// fakeGoogle is an OAuth provider: consent, token, userinfo and revoke endpoints.
type fakeGoogle struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	identity    string
	subject     string
	deny        bool
	echo        bool
	grant       func(requested []string) []string
	scopeReply  func(granted []string) (value any, present bool)
	tokenStatus int
	userinfoBad bool
	revokeFails bool
	onToken     func()
	codes       map[string]issuedCode
	issued      []string
	authorizes  []url.Values
	exchanges   []url.Values
	verifiers   []string
	revoked     []string
	pkceOK      int
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	f := &fakeGoogle{t: t, identity: workIdentity, subject: workSubject, codes: map[string]issuedCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", f.authorize)
	mux.HandleFunc("/token", f.token)
	mux.HandleFunc("/userinfo", f.userinfo)
	mux.HandleFunc("/revoke", f.revoke)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGoogle) authorize(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	f.authorizes = append(f.authorizes, q)
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || q.Get("client_id") != fakeClientID || q.Get("response_type") != "code" {
		http.Error(w, "bad authorize request", http.StatusBadRequest)
		return
	}
	reply := url.Values{"state": {q.Get("state")}}
	switch {
	case f.deny && f.echo:
		reply.Set("error", echoMarker+" "+q.Get("code_challenge"))
		reply.Set("error_description", echoMarker+" "+q.Encode())
	case f.deny:
		reply.Set("error", "access_denied")
		reply.Set("error_description", echoMarker+" the user said no")
	default:
		code := fmt.Sprintf("4/fake-code-%d", len(f.authorizes))
		f.codes[code] = issuedCode{challenge: q.Get("code_challenge"), redirectURI: q.Get("redirect_uri"), scopes: strings.Fields(q.Get("scope"))}
		f.issued = append(f.issued, code)
		reply.Set("code", code)
		reply.Set("scope", q.Get("scope"))
	}
	back.RawQuery = reply.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

// echoError answers like a provider whose diagnostics repeat the request it was sent.
func echoError(w http.ResponseWriter, status int, sent string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "invalid_request", "error_description": echoMarker + " " + sent, "error_uri": "https://x.example/?" + sent,
	})
}

func (f *fakeGoogle) token(w http.ResponseWriter, r *http.Request) {
	if f.onToken != nil {
		f.onToken()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	form := r.PostForm
	f.exchanges = append(f.exchanges, form)
	f.verifiers = append(f.verifiers, form.Get("code_verifier"))
	issued, ok := f.codes[form.Get("code")]
	delete(f.codes, form.Get("code"))
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	valid := ok && f.tokenStatus == 0 &&
		form.Get("grant_type") == "authorization_code" &&
		form.Get("client_id") == fakeClientID && form.Get("client_secret") == fakeClientSecret &&
		form.Get("redirect_uri") == issued.redirectURI &&
		base64.RawURLEncoding.EncodeToString(sum[:]) == issued.challenge
	if f.echo {
		echoError(w, http.StatusBadRequest, r.PostForm.Encode())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if !valid {
		status := f.tokenStatus
		if status == 0 {
			status = http.StatusBadRequest
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Bad Request"}`))
		return
	}
	f.pkceOK++
	granted := issued.scopes
	if f.grant != nil {
		granted = f.grant(granted)
	}
	reply := map[string]any{
		"access_token": fakeAccessToken, "refresh_token": fakeRefreshToken, "token_type": "Bearer",
		"expires_in": 3599, "scope": strings.Join(granted, " "),
	}
	if f.scopeReply != nil {
		if value, present := f.scopeReply(granted); present {
			reply["scope"] = value
		} else {
			delete(reply, "scope")
		}
	}
	_ = json.NewEncoder(w).Encode(reply)
}

func (f *fakeGoogle) userinfo(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.userinfoBad {
		echoError(w, http.StatusUnauthorized, r.Header.Get("Authorization"))
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+fakeAccessToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	info := map[string]any{"email": f.identity, "email_verified": true}
	if f.subject != "" {
		info["sub"] = f.subject
	}
	_ = json.NewEncoder(w).Encode(info)
}

func (f *fakeGoogle) revoke(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	f.revoked = append(f.revoked, r.PostForm.Get("token"))
	switch {
	case f.echo:
		echoError(w, http.StatusBadRequest, r.PostForm.Encode())
	case f.revokeFails:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	}
}

// consent plays the browser: it opens authorizeURL and returns where the provider redirected.
func (f *fakeGoogle) consent(authorizeURL string) string {
	f.t.Helper()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(authorizeURL)
	if err != nil {
		f.t.Fatalf("consent: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		f.t.Fatalf("consent: status %d", resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

// catalogFS is the shipped catalog with Google's endpoints pointed at the fake.
func (f *fakeGoogle) catalogFS() fs.FS {
	f.t.Helper()
	out := fstest.MapFS{}
	err := fs.WalkDir(shared.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(shared.FS, path)
		if err != nil {
			return err
		}
		if path == "providers/google.yaml" {
			data = bytes.ReplaceAll(data, []byte("https://accounts.google.com/o/oauth2/v2/auth"), []byte(f.srv.URL+"/authorize"))
			data = bytes.ReplaceAll(data, []byte("https://oauth2.googleapis.com/token"), []byte(f.srv.URL+"/token"))
			data = bytes.ReplaceAll(data, []byte("https://oauth2.googleapis.com/revoke"), []byte(f.srv.URL+"/revoke"))
			data = bytes.ReplaceAll(data, []byte("https://openidconnect.googleapis.com/v1/userinfo"), []byte(f.srv.URL+"/userinfo"))
		}
		out[path] = &fstest.MapFile{Data: data}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// harness is an initialised vault, a fake Google and the deps that wire addn to them.
// When the test ends it checks every command it ran for leaked secrets.
type harness struct {
	t      *testing.T
	home   string
	google *fakeGoogle
	clock  *fakeClock
	deps   deps

	mu         sync.Mutex
	results    []result
	challenges map[string]bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	home := isolatedHome(t)
	g := newFakeGoogle(t)
	clock := &fakeClock{t: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
	env := vault.OSEnv()
	env.Now = clock.Now
	h := &harness{t: t, home: home, google: g, clock: clock, challenges: map[string]bool{}}
	h.deps = deps{
		catalogFS:       g.catalogFS(),
		env:             env,
		httpClient:      &http.Client{Timeout: 5 * time.Second},
		openBrowser:     h.browse,
		loopbackTimeout: 5 * time.Second,
	}
	t.Cleanup(h.assertNothingLeaked)
	h.mustRun("init", "--key-file")
	return h
}

// browse is the default injected opener: a browser that consents and follows the redirect home.
func (h *harness) browse(authorizeURL string) <-chan error {
	exited := make(chan error, 1)
	exited <- h.visit(authorizeURL)
	return exited
}

func (h *harness) visit(authorizeURL string) error {
	resp, err := http.Get(authorizeURL)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

var challengeParam = regexp.MustCompile(`code_challenge=([A-Za-z0-9_-]{43})`)

// run records every PKCE challenge addn hands the browser or prints, so the leak check can
// recognise a verifier even when the provider never saw its authorization.
func (h *harness) run(args ...string) result {
	d := h.deps
	open := d.openBrowser
	d.openBrowser = func(authorizeURL string) <-chan error {
		h.noteChallenges(authorizeURL)
		return open(authorizeURL)
	}
	var stdout, stderr bytes.Buffer
	code := execute(args, &stdout, &stderr, d)
	r := result{args: args, code: code, stdout: stdout.String(), stderr: stderr.String()}
	h.noteChallenges(r.stdout + r.stderr)
	h.mu.Lock()
	h.results = append(h.results, r)
	h.mu.Unlock()
	return r
}

func (h *harness) noteChallenges(text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range challengeParam.FindAllStringSubmatch(text, -1) {
		h.challenges[m[1]] = true
	}
}

func (h *harness) mustRun(args ...string) result {
	h.t.Helper()
	r := h.run(args...)
	if r.code != ExitOK {
		h.t.Fatalf("addn %v: exit %d\nstdout: %s\nstderr: %s", args, r.code, r.stdout, r.stderr)
	}
	return r
}

func (h *harness) wantError(r result, exit int, code string) errorEnvelope {
	h.t.Helper()
	if r.code != exit {
		h.t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", r.code, exit, r.stdout, r.stderr)
	}
	env := decodeError(h.t, lastLine(r.stderr))
	if env.Error.Code != code {
		h.t.Errorf("error.code = %q, want %q (message %q)", env.Error.Code, code, env.Error.Message)
	}
	return env
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

func (h *harness) addClient() {
	h.t.Helper()
	h.mustRun("client", "add", "google", "--file", writeClientFile(h.t, "installed"))
}

var base64URLRun = regexp.MustCompile(`[A-Za-z0-9_-]{43,}`)

// assertNothingLeaked checks every command this harness ran for the client secret, the tokens,
// every authorization code, echoed provider diagnostics, and any PKCE verifier, including one
// that never reached the token endpoint: no 43-character run may hash to an issued challenge.
func (h *harness) assertNothingLeaked() {
	h.t.Helper()
	secrets := []string{fakeClientSecret, fakeAccessToken, fakeRefreshToken, echoMarker}
	h.mu.Lock()
	defer h.mu.Unlock()
	challenges := h.challenges
	h.google.mu.Lock()
	for _, v := range h.google.verifiers {
		if v != "" {
			secrets = append(secrets, v)
		}
	}
	secrets = append(secrets, h.google.issued...)
	for _, q := range h.google.authorizes {
		challenges[q.Get("code_challenge")] = true
	}
	h.google.mu.Unlock()

	for _, r := range h.results {
		assertNoLeak(h.t, r, secrets...)
		for stream, out := range map[string]string{"stdout": r.stdout, "stderr": r.stderr} {
			for _, run := range base64URLRun.FindAllString(out, -1) {
				for i := 0; i+43 <= len(run); i++ {
					sum := sha256.Sum256([]byte(run[i : i+43]))
					if challenges[base64.RawURLEncoding.EncodeToString(sum[:])] {
						h.t.Errorf("a PKCE verifier leaked into the %s of `%s`", stream, r.command())
					}
				}
			}
		}
	}
}

func writeClientFile(t *testing.T, kind string) string {
	t.Helper()
	body := map[string]any{kind: map[string]any{
		"client_id":     fakeClientID,
		"client_secret": fakeClientSecret,
		"project_id":    "addn-test",
		"auth_uri":      "https://accounts.google.com/o/oauth2/auth",
		"token_uri":     "https://oauth2.googleapis.com/token",
		"redirect_uris": []string{"http://localhost"},
	}}
	data, _ := json.Marshal(body)
	path := filepath.Join(t.TempDir(), "client_secret.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func decodeJSON[T any](t *testing.T, r result) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("stdout is not the expected JSON: %v\nstdout: %q", err, r.stdout)
	}
	return v
}
