package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

type apiRequest struct {
	Method, Path, RawQuery string
	Header                 http.Header
	Body                   string
}

// fakeAPI is a TLS Google API: it records requests and answers each with its handler.
type fakeAPI struct {
	srv *httptest.Server

	mu      sync.Mutex
	reqs    []apiRequest
	handler func(n int, w http.ResponseWriter, r *http.Request)
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	a := &fakeAPI{handler: jsonBody(`{"ok":true}`)}
	a.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.reqs = append(a.reqs, apiRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(body)})
		n, handler := len(a.reqs), a.handler
		a.mu.Unlock()
		handler(n, w, r)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *fakeAPI) respond(handler func(n int, w http.ResponseWriter, r *http.Request)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handler = handler
}

func (a *fakeAPI) requests() []apiRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]apiRequest(nil), a.reqs...)
}

func (a *fakeAPI) host() string { return strings.TrimPrefix(a.srv.URL, "https://") }

func (a *fakeAPI) url(path string) string { return a.srv.URL + path }

func jsonBody(body string) func(int, http.ResponseWriter, *http.Request) {
	return status(http.StatusOK, body, "Content-Type", "application/json; charset=UTF-8")
}

func status(code int, body string, headers ...string) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

// requireToken answers 200 to the token and 401 to any other.
func requireToken(token string) func(int, http.ResponseWriter, *http.Request) {
	return func(n int, w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			status(http.StatusUnauthorized, `{"error":{"code":401}}`, "Content-Type", "application/json")(n, w, r)
			return
		}
		jsonBody(`{"ok":true}`)(n, w, r)
	}
}

// recordingTransport notes the host of every request that leaves addn.
type recordingTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	seen []string
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.seen = append(t.seen, req.URL.Host)
	t.mu.Unlock()
	return t.base.RoundTrip(req)
}

func (t *recordingTransport) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen = nil
}

func (t *recordingTransport) total() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.seen)
}

// callHarness is a harness with a connected gmail:work account, a TLS fake API that the
// catalog names as Gmail's API host, and a second TLS server that no catalog entry allows.
type callHarness struct {
	*harness
	api, evil *fakeAPI
	transport *recordingTransport

	sleepMu sync.Mutex
	sleeps  []time.Duration
}

func newCallHarness(t *testing.T, tweak ...func(*fakeGoogle)) *callHarness {
	t.Helper()
	h := newHarness(t)
	h.addClient()
	for _, fn := range tweak {
		h.google.configure(fn)
	}
	c := &callHarness{harness: h, api: newFakeAPI(t), evil: newFakeAPI(t)}
	c.transport = &recordingTransport{base: c.api.srv.Client().Transport}
	h.deps.catalogFS = pointAppAPIAt(t, h.deps.catalogFS, "gmail", c.api.host())
	h.deps.httpClient = &http.Client{Transport: c.transport, Timeout: 5 * time.Second}
	h.deps.sleep = c.recordSleep
	h.connect("work", "gmail")
	c.transport.reset()
	return c
}

// attachApp connects the alias under another app, with its own identity subject, and points
// that app's API at a fake server of its own.
func (c *callHarness) attachApp(app, alias, subject string) *fakeAPI {
	c.t.Helper()
	api := newFakeAPI(c.t)
	c.deps.catalogFS = pointAppAPIAt(c.t, c.deps.catalogFS, app, api.host())
	c.google.configure(func(g *fakeGoogle) { g.subject = subject })
	c.connect(alias, app)
	c.google.configure(func(g *fakeGoogle) { g.subject = workSubject })
	c.transport.reset()
	return api
}

// attachRoutedApp is attachApp on a server that other apps may share, with the app's own routes enforced.
func (c *callHarness) attachRoutedApp(app, alias, subject string, api *fakeAPI) {
	c.t.Helper()
	c.deps.catalogFS = pointAppRoutesAt(c.t, c.deps.catalogFS, app, api.host())
	c.google.configure(func(g *fakeGoogle) { g.subject = subject })
	c.connect(alias, app)
	c.google.configure(func(g *fakeGoogle) { g.subject = workSubject })
	c.transport.reset()
}

func (c *callHarness) recordSleep(_ context.Context, d time.Duration) error {
	c.sleepMu.Lock()
	defer c.sleepMu.Unlock()
	c.sleeps = append(c.sleeps, d)
	return nil
}

func (c *callHarness) slept() []time.Duration {
	c.sleepMu.Lock()
	defer c.sleepMu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

func (c *callHarness) call(args ...string) result {
	return c.run(append([]string{"call", "gmail"}, args...)...)
}

func (c *callHarness) callWithStdin(stdin string, args ...string) result {
	return c.runWithStdin(strings.NewReader(stdin), append([]string{"call", "gmail"}, args...)...)
}

// expireToken moves the clock past the stored access token's lifetime.
func (c *callHarness) expireToken() { c.clock.Advance(2 * time.Hour) }

// wantUpstream checks the upstream-failure contract: stdout is the upstream body, stderr
// ends in an envelope with only the status.
func (c *callHarness) wantUpstream(r result, exit, httpStatus int, body string) {
	c.t.Helper()
	env := c.wantError(r, exit, "upstream-error")
	if want := "HTTP " + strconv.Itoa(httpStatus); env.Error.Message != want {
		c.t.Errorf("message = %q, want %q", env.Error.Message, want)
	}
	if r.stdout != body {
		c.t.Errorf("stdout = %q, want the upstream body %q", r.stdout, body)
	}
	if lines := strings.Split(strings.TrimRight(r.stderr, "\n"), "\n"); len(lines) != 1 {
		c.t.Errorf("stderr has %d lines, want just the envelope: %q", len(lines), r.stderr)
	}
}

var baseURLLine = regexp.MustCompile(`(?m)^  base_url: ".*"$`)

// pointAppAPIAt names host as the app's API host and lists it as an allowed one. It also lets
// every path through, so tests need not use the app's real routes; pointAppRoutesAt keeps them.
func pointAppAPIAt(t *testing.T, src fs.FS, app, host string) fs.FS {
	t.Helper()
	return editProvider(t, pointAppRoutesAt(t, src, app, host), app, func(data []byte) []byte {
		return pathPrefixBlock.ReplaceAll(data, []byte("  allowed_path_prefixes:\n    - \"/\"\n"))
	})
}

var pathPrefixBlock = regexp.MustCompile(`(?m)^  allowed_path_prefixes:\n(?:    - ".*"\n)+`)

// pointAppRoutesAt is pointAppAPIAt with the app's own path prefixes left in force.
func pointAppRoutesAt(t *testing.T, src fs.FS, app, host string) fs.FS {
	t.Helper()
	return editProvider(t, src, app, func(data []byte) []byte {
		data = allowHost(data, host)
		return baseURLLine.ReplaceAll(data, []byte(`  base_url: "https://`+host+`"`))
	})
}

// editProvider applies edit to providers/<id>.yaml of src.
func editProvider(t *testing.T, src fs.FS, id string, edit func([]byte) []byte) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	err := fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}
		if path == "providers/"+id+".yaml" {
			data = edit(data)
		}
		out[path] = &fstest.MapFile{Data: data}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustReplace(t *testing.T, data []byte, old, replacement string) []byte {
	t.Helper()
	if !bytes.Contains(data, []byte(old)) {
		t.Fatalf("the provider YAML has no line %q", old)
	}
	return bytes.Replace(data, []byte(old), []byte(replacement), 1)
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// replaceInProvider replaces old with replacement in providers/google.yaml of src.
func replaceInProvider(t *testing.T, src fs.FS, old, replacement string) fs.FS {
	t.Helper()
	return editProvider(t, src, "google", func(data []byte) []byte { return mustReplace(t, data, old, replacement) })
}

// lastStdout is what the harness's most recent command printed.
func (h *harness) lastStdout() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.results[len(h.results)-1].stdout
}
