package proxy_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/proxy"
)

// closeWithoutAnswering drops the connection after reading the request, as a crashing server does.
func closeWithoutAnswering(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func TestEngineNeverLetsTheTransportReplayARequestAfterALostConnection(t *testing.T) {
	cases := []struct {
		name, method, body string
		header             http.Header
	}{
		{"POST with Idempotency-Key", "POST", `{}`, http.Header{"Idempotency-Key": {"k"}}},
		{"POST with X-Idempotency-Key", "POST", `{}`, http.Header{"X-Idempotency-Key": {"k"}}},
		{"POST without a body and an Idempotency-Key", "POST", "", http.Header{"Idempotency-Key": {"k"}}},
		{"PUT with Idempotency-Key", "PUT", `{}`, http.Header{"Idempotency-Key": {"k"}}},
		{"POST without a key", "POST", `{}`, nil},
		{"GET", "GET", "", nil},
		{"DELETE with Idempotency-Key", "DELETE", "", http.Header{"Idempotency-Key": {"k"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t, func(n int, w http.ResponseWriter, _ *http.Request) {
				switch n {
				case 1:
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(429)
				case 2:
					closeWithoutAnswering(w)
				default:
					_, _ = w.Write([]byte("replayed"))
				}
			})
			e := newEngine(&fakeRenewer{}, &sleeps{})

			_, err := e.Do(context.Background(), proxy.Request{Method: tc.method, URL: up.url("/x"), Header: tc.header, Body: []byte(tc.body)}, "t")

			var nerr *proxy.NetworkError
			if !errors.As(err, &nerr) {
				t.Errorf("err = %v, want a NetworkError after the connection was lost", err)
			}
			if n := len(up.seen()); n != 2 {
				t.Errorf("the server saw %d requests, want 2: one throttled, one deliberate retry, never a transport replay", n)
			}
		})
	}
}

func TestEngineDoesNotWaitForTheBodyOfARejectedResponse(t *testing.T) {
	for _, rejected := range []int{401, 429} {
		t.Run(strconv.Itoa(rejected), func(t *testing.T) {
			release := make(chan struct{})
			up := newUpstream(t, func(n int, w http.ResponseWriter, _ *http.Request) {
				if n > 1 {
					_, _ = w.Write([]byte("done"))
					return
				}
				w.Header().Set("Retry-After", "0")
				w.Header().Set("Content-Length", "1000")
				w.WriteHeader(rejected)
				w.(http.Flusher).Flush()
				<-release
			})
			t.Cleanup(func() { close(release) })
			e := newEngine(&fakeRenewer{token: newAccess}, &sleeps{})
			e.Client = &http.Client{Timeout: time.Minute}
			finished := make(chan *proxy.Response, 1)

			go func() {
				resp, _ := e.Do(context.Background(), proxy.Request{Method: "GET", URL: up.url("/")}, oldAccess)
				finished <- resp
			}()

			select {
			case resp := <-finished:
				if resp == nil || resp.Status != 200 {
					t.Errorf("response = %+v, want the retry's 200", resp)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Do is stuck draining the rejected response's body")
			}
		})
	}
}

func TestEngineSaturatesHugeRetryAfterValuesAtTheCap(t *testing.T) {
	for _, header := range []string{"4294967296", "4294967295", "18446744073709551615", "18446744073709551616", "99999999999999999999999999"} {
		up := newUpstream(t, func(n int, w http.ResponseWriter, _ *http.Request) {
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
			t.Errorf("Retry-After %s: sleeps = %v, want [30s]", header, got)
		}
	}
}

type countingTransport struct {
	base  http.RoundTripper
	calls atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.base.RoundTrip(r)
}

func TestEngineReturnsARedirectWhateverItsLocation(t *testing.T) {
	for _, location := range []string{"https://%zz/", "/other/path", "", "http://[::1"} {
		t.Run(location, func(t *testing.T) {
			up := newUpstream(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", location)
				w.WriteHeader(302)
			})
			e := newEngine(&fakeRenewer{}, &sleeps{})
			transport := &countingTransport{base: http.DefaultTransport}
			e.Client = &http.Client{Transport: transport}

			resp, err := e.Do(context.Background(), proxy.Request{Method: "GET", URL: up.url("/")}, oldAccess)

			if err != nil || resp.Status != 302 {
				t.Fatalf("Do = %+v, %v; want the 302 itself", resp, err)
			}
			_ = resp.Body.Close()
			if transport.calls.Load() != 1 || len(up.seen()) != 1 {
				t.Errorf("transport calls %d, server requests %d; a redirect is never followed", transport.calls.Load(), len(up.seen()))
			}
		})
	}
}

func TestEngineReportsWhatWasSentWhenCancelledWhileWaiting(t *testing.T) {
	up := newUpstream(t, reply(429, "slow"))
	ctx, cancel := context.WithCancel(context.Background())
	e := newEngine(&fakeRenewer{}, &sleeps{})
	e.Sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }

	_, err := e.Do(ctx, proxy.Request{Method: "GET", URL: up.url("/")}, "t")

	var attempted *proxy.Attempted
	if !errors.As(err, &attempted) || attempted.Status != 429 || !errors.Is(err, context.Canceled) {
		t.Errorf("err = %#v, want Attempted{Status: 429} wrapping context.Canceled", err)
	}
}

func TestEngineReportsStatusZeroWhenNothingAnswered(t *testing.T) {
	up := newUpstream(t, reply(200, ""))
	target := up.url("/")
	up.srv.Close()

	_, err := newEngine(&fakeRenewer{}, &sleeps{}).Do(context.Background(), proxy.Request{Method: "GET", URL: target}, "t")

	var attempted *proxy.Attempted
	var nerr *proxy.NetworkError
	if !errors.As(err, &attempted) || attempted.Status != 0 || !errors.As(err, &nerr) {
		t.Errorf("err = %#v, want Attempted{Status: 0} wrapping a NetworkError", err)
	}
}

func TestEngineTreatsAnOverflowingValueWithASuffixAsMalformed(t *testing.T) {
	for _, header := range []string{"999999999999999999999999X", "99999999999999999999999 9", "-99999999999999999999999", "99999999999999999999999.5"} {
		up := newUpstream(t, func(n int, w http.ResponseWriter, _ *http.Request) {
			if n == 1 {
				w.Header().Set("Retry-After", header)
				w.WriteHeader(429)
			}
		})
		s := &sleeps{}
		e := newEngine(&fakeRenewer{}, s)
		e.Jitter = func(time.Duration) time.Duration { return 0 }

		if _, _, err := do(t, e, proxy.Request{Method: "GET", URL: up.url("/")}, "t"); err != nil {
			t.Fatal(err)
		}

		if got := s.all(); len(got) != 1 || got[0] != 500*time.Millisecond {
			t.Errorf("Retry-After %q: sleeps = %v, want the jittered fallback [500ms], not the 30s cap", header, got)
		}
	}
}
