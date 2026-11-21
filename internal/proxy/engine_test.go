package proxy_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/proxy"
)

type seenRequest struct {
	method, path, rawQuery, auth, contentType string
	header                                    http.Header
	body                                      string
}

// upstream is a fake API: it records requests and answers each with handle.
type upstream struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []seenRequest
	handle   func(n int, w http.ResponseWriter, r *http.Request)
}

func newUpstream(t *testing.T, handle func(n int, w http.ResponseWriter, r *http.Request)) *upstream {
	t.Helper()
	u := &upstream{handle: handle}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.requests = append(u.requests, seenRequest{
			method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery, auth: r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"), header: r.Header.Clone(), body: string(body),
		})
		n := len(u.requests)
		u.mu.Unlock()
		u.handle(n, w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) seen() []seenRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]seenRequest(nil), u.requests...)
}

func (u *upstream) url(path string) *url.URL {
	parsed, _ := url.Parse(u.srv.URL + path)
	return parsed
}

func reply(status int, body string, headers ...string) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

type fakeRenewer struct {
	calls    []string
	token    string
	err      error
	rejected string
}

func (r *fakeRenewer) Renew(_ context.Context, rejected string) (string, error) {
	r.calls = append(r.calls, rejected)
	return r.token, r.err
}

type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d = append(s.d, d)
	return nil
}

func (s *sleeps) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

func newEngine(renewer proxy.Renewer, s *sleeps) *proxy.Engine {
	return &proxy.Engine{
		Client:  http.DefaultClient,
		API:     catalog.API{AuthHeader: "Authorization", AuthPrefix: "Bearer"},
		Renewer: renewer,
		Now:     func() time.Time { return t0 },
		Sleep:   s.sleep,
	}
}

func do(t *testing.T, e *proxy.Engine, req proxy.Request, token string) (*proxy.Response, string, error) {
	t.Helper()
	resp, err := e.Do(context.Background(), req, token)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body), nil
}

func TestEngineInjectsTheTokenAndSendsTheRequest(t *testing.T) {
	up := newUpstream(t, reply(200, `{"ok":true}`, "Content-Type", "application/json"))
	req := proxy.Request{
		Method: "POST", URL: up.url("/v1/things?a=1"), Body: []byte(`{"name":"x"}`),
		Header: http.Header{"X-Goog-User-Project": {"p"}, "Authorization": {"Bearer forged"}},
	}

	resp, body, err := do(t, newEngine(&fakeRenewer{}, &sleeps{}), req, oldAccess)

	if err != nil || resp.Status != 200 || body != `{"ok":true}` {
		t.Fatalf("Do = %v, %q, %v", resp, body, err)
	}
	got := up.seen()[0]
	if got.method != "POST" || got.path != "/v1/things" || got.rawQuery != "a=1" || got.body != `{"name":"x"}` {
		t.Errorf("request = %+v", got)
	}
	if got.auth != "Bearer "+oldAccess {
		t.Errorf("Authorization = %q, want the injected token to win over any caller header", got.auth)
	}
	if got.contentType != "application/json" || got.header.Get("X-Goog-User-Project") != "p" {
		t.Errorf("headers = %v", got.header)
	}
}

func TestEngineUsesTheProvidersAuthHeader(t *testing.T) {
	up := newUpstream(t, reply(200, ""))
	e := newEngine(&fakeRenewer{}, &sleeps{})
	e.API = catalog.API{AuthHeader: "X-Api-Key", AuthPrefix: ""}

	if _, _, err := do(t, e, proxy.Request{Method: "GET", URL: up.url("/")}, "k1"); err != nil {
		t.Fatal(err)
	}

	got := up.seen()[0].header
	if got.Get("X-Api-Key") != "k1" || got.Get("Authorization") != "" {
		t.Errorf("headers = %v", got)
	}
}

func TestEngineKeepsAnExplicitContentTypeAndDefaultsJSONForBodies(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		header http.Header
		want   string
	}{
		{"default for a body", `{}`, nil, "application/json"},
		{"caller's wins", `a,b`, http.Header{"Content-Type": {"text/csv"}}, "text/csv"},
		{"none without a body", "", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t, reply(200, ""))

			_, _, err := do(t, newEngine(&fakeRenewer{}, &sleeps{}), proxy.Request{Method: "POST", URL: up.url("/"), Body: []byte(tc.body), Header: tc.header}, "t")

			if err != nil {
				t.Fatal(err)
			}
			if got := up.seen()[0].contentType; got != tc.want {
				t.Errorf("Content-Type = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEngineSendsEveryMethodIncludingHEAD(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		up := newUpstream(t, reply(200, "body"))

		resp, body, err := do(t, newEngine(&fakeRenewer{}, &sleeps{}), proxy.Request{Method: method, URL: up.url("/")}, "t")

		if err != nil || resp.Status != 200 || up.seen()[0].method != method {
			t.Errorf("%s: %v %v, saw %+v", method, resp, err, up.seen())
		}
		if method == "HEAD" && body != "" {
			t.Errorf("HEAD body = %q", body)
		}
	}
}

func TestEngineRetries429HonouringRetryAfterSeconds(t *testing.T) {
	up := newUpstream(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n <= 2 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			return
		}
		_, _ = w.Write([]byte("done"))
	})
	s := &sleeps{}

	resp, body, err := do(t, newEngine(&fakeRenewer{}, s), proxy.Request{Method: "POST", URL: up.url("/x"), Body: []byte("payload")}, oldAccess)

	if err != nil || resp.Status != 200 || body != "done" {
		t.Fatalf("Do = %v, %q, %v", resp, body, err)
	}
	if got := s.all(); len(got) != 2 || got[0] != 2*time.Second || got[1] != 2*time.Second {
		t.Errorf("sleeps = %v, want [2s 2s]", got)
	}
	seen := up.seen()
	if len(seen) != 3 {
		t.Fatalf("requests = %d, want 3", len(seen))
	}
	for i, r := range seen {
		if r.auth != "Bearer "+oldAccess || r.body != "payload" {
			t.Errorf("attempt %d sent auth %q, body %q; a retry must replay the same request", i, r.auth, r.body)
		}
	}
}

func TestEngineHonoursRetryAfterHTTPDates(t *testing.T) {
	up := newUpstream(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			w.Header().Set("Retry-After", t0.Add(7*time.Second).UTC().Format(http.TimeFormat))
			w.WriteHeader(429)
		}
	})
	s := &sleeps{}

	if _, _, err := do(t, newEngine(&fakeRenewer{}, s), proxy.Request{Method: "GET", URL: up.url("/")}, "t"); err != nil {
		t.Fatal(err)
	}

	if got := s.all(); len(got) != 1 || got[0] != 7*time.Second {
		t.Errorf("sleeps = %v, want [7s]", got)
	}
}

func TestEngineCapsEachWaitAtThirtySeconds(t *testing.T) {
	for _, header := range []string{"3600", t0.Add(24 * time.Hour).UTC().Format(http.TimeFormat)} {
		up := newUpstream(t, func(n int, w http.ResponseWriter, r *http.Request) {
			if n == 1 {
				w.Header().Set("Retry-After", header)
				w.WriteHeader(429)
			}
		})
		s := &sleeps{}

		if _, _, err := do(t, newEngine(&fakeRenewer{}, s), proxy.Request{Method: "GET", URL: up.url("/")}, "t"); err != nil {
			t.Fatal(err)
		}

		if got := s.all(); len(got) != 1 || got[0] != 30*time.Second {
			t.Errorf("Retry-After %q: sleeps = %v, want [30s]", header, got)
		}
	}
}

func TestEngineBacksOffExponentiallyWithJitterWithoutRetryAfter(t *testing.T) {
	for _, header := range []string{"", "soon", "-5", "1.5"} {
		up := newUpstream(t, reply(429, "slow down", "Retry-After", header))
		s := &sleeps{}

		resp, body, err := do(t, newEngine(&fakeRenewer{}, s), proxy.Request{Method: "GET", URL: up.url("/")}, "t")

		if err != nil || resp.Status != 429 || body != "slow down" {
			t.Fatalf("Retry-After %q: Do = %v, %q, %v; want the final 429 handed back", header, resp, body, err)
		}
		if n := len(up.seen()); n != 4 {
			t.Errorf("Retry-After %q: requests = %d, want 1 + 3 retries", header, n)
		}
		got := s.all()
		if len(got) != 3 {
			t.Fatalf("Retry-After %q: sleeps = %v, want 3", header, got)
		}
		for i, ceiling := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
			if got[i] < ceiling/2 || got[i] > ceiling {
				t.Errorf("Retry-After %q: wait %d = %s, want within [%s, %s]", header, i, got[i], ceiling/2, ceiling)
			}
		}
	}
}

func TestEngineDoesNotRetryServerErrors(t *testing.T) {
	up := newUpstream(t, reply(503, "down"))
	s := &sleeps{}

	resp, _, err := do(t, newEngine(&fakeRenewer{}, s), proxy.Request{Method: "GET", URL: up.url("/")}, "t")

	if err != nil || resp.Status != 503 || len(up.seen()) != 1 || len(s.all()) != 0 {
		t.Errorf("status %v, %d requests, sleeps %v; want one request and no retry", resp, len(up.seen()), s.all())
	}
}

func TestEngineRefreshesOnceOn401AndRetries(t *testing.T) {
	up := newUpstream(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+newAccess {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte("fresh"))
	})
	renewer := &fakeRenewer{token: newAccess}

	resp, body, err := do(t, newEngine(renewer, &sleeps{}), proxy.Request{Method: "POST", URL: up.url("/"), Body: []byte("b")}, oldAccess)

	if err != nil || resp.Status != 200 || body != "fresh" {
		t.Fatalf("Do = %v, %q, %v", resp, body, err)
	}
	if len(renewer.calls) != 1 || renewer.calls[0] != oldAccess {
		t.Errorf("Renew calls = %v, want one call naming the rejected token", renewer.calls)
	}
	if seen := up.seen(); len(seen) != 2 || seen[1].body != "b" {
		t.Errorf("requests = %+v, want 2 with the body replayed", seen)
	}
}

func TestEngineSurfacesASecond401(t *testing.T) {
	up := newUpstream(t, reply(401, `{"error":"nope"}`))
	renewer := &fakeRenewer{token: newAccess}

	resp, body, err := do(t, newEngine(renewer, &sleeps{}), proxy.Request{Method: "GET", URL: up.url("/")}, oldAccess)

	if err != nil || resp.Status != 401 || body != `{"error":"nope"}` {
		t.Fatalf("Do = %v, %q, %v; want the second 401 handed back", resp, body, err)
	}
	if len(renewer.calls) != 1 || len(up.seen()) != 2 {
		t.Errorf("renews %d, requests %d; want exactly 1 and 2: never a loop", len(renewer.calls), len(up.seen()))
	}
}

func TestEngineReportsWhyA401CouldNotBeRenewed(t *testing.T) {
	for name, renewErr := range map[string]error{"no refresh token": proxy.ErrNoRefreshToken, "needs reauth": proxy.ErrNeedsReauth} {
		t.Run(name, func(t *testing.T) {
			up := newUpstream(t, reply(401, "no"))

			_, err := newEngine(&fakeRenewer{err: renewErr}, &sleeps{}).Do(context.Background(), proxy.Request{Method: "GET", URL: up.url("/")}, oldAccess)

			var failure *proxy.AuthFailure
			if !errors.As(err, &failure) || failure.Status != 401 || !errors.Is(err, renewErr) {
				t.Fatalf("err = %#v, want an AuthFailure for status 401 wrapping %v", err, renewErr)
			}
			if len(up.seen()) != 1 {
				t.Errorf("requests = %d, want 1", len(up.seen()))
			}
		})
	}
}

func TestEngineCountsThrottlesAndRefreshesIndependently(t *testing.T) {
	up := newUpstream(t, func(n int, w http.ResponseWriter, r *http.Request) {
		switch {
		case n == 1:
			w.WriteHeader(401)
		case n == 2:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
		}
	})
	renewer := &fakeRenewer{token: newAccess}

	resp, _, err := do(t, newEngine(renewer, &sleeps{}), proxy.Request{Method: "GET", URL: up.url("/")}, oldAccess)

	if err != nil || resp.Status != 200 {
		t.Fatalf("Do = %v, %v", resp, err)
	}
	if seen := up.seen(); len(seen) != 3 || seen[1].auth != "Bearer "+newAccess || seen[2].auth != "Bearer "+newAccess {
		t.Errorf("requests = %+v, want the renewed token on the retry and on the throttle retry", seen)
	}
}

func TestEngineNeverFollowsRedirects(t *testing.T) {
	elsewhere := newUpstream(t, reply(200, "stolen"))
	up := newUpstream(t, reply(302, "", "Location", elsewhere.srv.URL+"/landing"))

	resp, _, err := do(t, newEngine(&fakeRenewer{}, &sleeps{}), proxy.Request{Method: "GET", URL: up.url("/")}, oldAccess)

	if err != nil || resp.Status != 302 {
		t.Fatalf("Do = %v, %v; want the 302 itself", resp, err)
	}
	if n := len(elsewhere.seen()); n != 0 {
		t.Errorf("the redirect target received %d requests", n)
	}
}

func TestEngineNetworkErrorsAreLocallyAuthored(t *testing.T) {
	up := newUpstream(t, reply(200, ""))
	target := up.url("/secret/path?token=QUERYSECRET")
	up.srv.Close()
	e := newEngine(&fakeRenewer{}, &sleeps{})

	_, err := e.Do(context.Background(), proxy.Request{Method: "GET", URL: target, Header: http.Header{"X-Secret": {"HEADERSECRET"}}}, oldAccess)

	var nerr *proxy.NetworkError
	if !errors.As(err, &nerr) {
		t.Fatalf("err = %#v, want a NetworkError", err)
	}
	for _, leak := range []string{"QUERYSECRET", "HEADERSECRET", oldAccess, "secret/path", up.srv.URL, "127.0.0.1"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the error repeats %q: %v", leak, err)
		}
	}
}

func TestEngineStopsWaitingWhenCancelled(t *testing.T) {
	up := newUpstream(t, reply(429, ""))
	ctx, cancel := context.WithCancel(context.Background())
	e := newEngine(&fakeRenewer{}, &sleeps{})
	e.Sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }

	_, err := e.Do(ctx, proxy.Request{Method: "GET", URL: up.url("/")}, "t")

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestEngineGivesUpOnAServerThatNeverAnswers(t *testing.T) {
	up := newUpstream(t, func(_ int, _ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	e := newEngine(&fakeRenewer{}, &sleeps{})
	e.Client = &http.Client{Timeout: time.Minute}
	var waited time.Duration
	e.AfterFunc = func(d time.Duration, expire func()) (stop func() bool) {
		waited = d
		go expire()
		return func() bool { return true }
	}

	_, err := e.Do(context.Background(), proxy.Request{Method: "GET", URL: up.url("/")}, "t")

	var nerr *proxy.NetworkError
	if !errors.As(err, &nerr) {
		t.Errorf("err = %#v, want a NetworkError once the header timeout expires", err)
	}
	if waited != time.Minute {
		t.Errorf("header timeout = %s, want the client's 1m", waited)
	}
}

func TestEngineReportsEveryAccessTokenItSent(t *testing.T) {
	up := newUpstream(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			w.WriteHeader(401)
		}
	})
	renewer := &fakeRenewer{token: newAccess}

	resp, _, err := do(t, newEngine(renewer, &sleeps{}), proxy.Request{Method: "GET", URL: up.url("/")}, oldAccess)

	if err != nil || !slices.Equal(resp.Tokens, []string{oldAccess, newAccess}) {
		t.Errorf("Tokens = %v, %v; want the rejected and the renewed token", resp.Tokens, err)
	}
}
