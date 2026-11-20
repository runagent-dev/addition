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
	deny        bool
	grant       func(requested []string) []string
	tokenStatus int
	revokeFails bool
	codes       map[string]issuedCode
	authorizes  []url.Values
	exchanges   []url.Values
	verifiers   []string
	revoked     []string
	pkceOK      int
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	f := &fakeGoogle{t: t, identity: workIdentity, codes: map[string]issuedCode{}}
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
	if f.deny {
		reply.Set("error", "access_denied")
	} else {
		code := fmt.Sprintf("4/fake-code-%d", len(f.authorizes))
		f.codes[code] = issuedCode{challenge: q.Get("code_challenge"), redirectURI: q.Get("redirect_uri"), scopes: strings.Fields(q.Get("scope"))}
		reply.Set("code", code)
		reply.Set("scope", q.Get("scope"))
	}
	back.RawQuery = reply.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (f *fakeGoogle) token(w http.ResponseWriter, r *http.Request) {
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
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": fakeAccessToken, "refresh_token": fakeRefreshToken, "token_type": "Bearer",
		"expires_in": 3599, "scope": strings.Join(granted, " "),
	})
}

func (f *fakeGoogle) userinfo(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+fakeAccessToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"sub": "1001", "email": f.identity, "email_verified": true})
}

func (f *fakeGoogle) revoke(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	f.revoked = append(f.revoked, r.PostForm.Get("token"))
	if f.revokeFails {
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
type harness struct {
	t       *testing.T
	home    string
	google  *fakeGoogle
	clock   *fakeClock
	deps    deps
	results []result
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	home := isolatedHome(t)
	g := newFakeGoogle(t)
	clock := &fakeClock{t: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
	env := vault.OSEnv()
	env.Now = clock.Now
	h := &harness{t: t, home: home, google: g, clock: clock}
	h.deps = deps{
		catalogFS:       g.catalogFS(),
		env:             env,
		httpClient:      &http.Client{Timeout: 5 * time.Second},
		openBrowser:     h.browse,
		loopbackTimeout: 5 * time.Second,
	}
	h.mustRun("init", "--key-file")
	return h
}

// browse is the default injected opener: a browser that consents and follows the redirect home.
func (h *harness) browse(authorizeURL string) error {
	resp, err := http.Get(authorizeURL)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (h *harness) run(args ...string) result {
	var stdout, stderr bytes.Buffer
	code := execute(args, &stdout, &stderr, h.deps)
	r := result{code: code, stdout: stdout.String(), stderr: stderr.String()}
	h.results = append(h.results, r)
	return r
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
	env := decodeError(h.t, r.stderr)
	if env.Error.Code != code {
		h.t.Errorf("error.code = %q, want %q (message %q)", env.Error.Code, code, env.Error.Message)
	}
	return env
}

func (h *harness) addClient() {
	h.t.Helper()
	h.mustRun("client", "add", "google", "--file", writeClientFile(h.t, "installed"))
}

// assertNothingLeaked checks every command this harness ran for secrets, PKCE verifiers included.
func (h *harness) assertNothingLeaked() {
	h.t.Helper()
	secrets := []string{fakeClientSecret, fakeAccessToken, fakeRefreshToken}
	h.google.mu.Lock()
	for _, v := range h.google.verifiers {
		if v != "" {
			secrets = append(secrets, v)
		}
	}
	h.google.mu.Unlock()
	for _, r := range h.results {
		assertNoLeak(h.t, r, secrets...)
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
