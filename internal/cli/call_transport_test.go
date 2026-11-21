package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"slices"
	"testing"
	"time"
)

func TestCallNeverReplaysAPostAfterTheConnectionIsLost(t *testing.T) {
	for _, key := range []string{"Idempotency-Key", "X-Idempotency-Key"} {
		t.Run(key, func(t *testing.T) {
			c := newCallHarness(t)
			c.api.respond(func(n int, w http.ResponseWriter, r *http.Request) {
				switch n {
				case 1:
					status(429, "", "Retry-After", "0")(n, w, r)
				case 2:
					conn, _, _ := w.(http.Hijacker).Hijack()
					_ = conn.Close()
				default:
					jsonBody(`{"replayed":true}`)(n, w, r)
				}
			})

			r := c.call("-a", "work", "POST", "/x", "--data", "{}", "-H", key+": test-key")

			c.wantError(r, ExitUpstreamServer, "network")
			if n := len(c.api.requests()); n != 2 {
				t.Errorf("the API saw %d requests, want exactly 2 (the throttled one and its retry)", n)
			}
		})
	}
}

func TestCallReportsARedirectWhateverItsLocation(t *testing.T) {
	for name, location := range map[string]string{
		"malformed":     "https://%zz/",
		"same host":     "/drive/v3/other",
		"another host":  "", // filled below with the evil server
		"absent":        "-",
		"unparseable v": "http://[::1",
	} {
		t.Run(name, func(t *testing.T) {
			c := newCallHarness(t)
			switch location {
			case "":
				location = c.evil.url("/x")
			case "-":
				location = ""
			}
			headers := []string{"Content-Type", "text/plain"}
			if location != "" {
				headers = append(headers, "Location", location)
			}
			c.api.respond(status(302, "moved", headers...))

			r := c.call("-a", "work", "GET", "/x")

			c.wantUpstream(r, ExitUpstreamClient, 302, "")
			if got := c.transport.total(); got != 1 {
				t.Errorf("transport calls = %d, want 1: a redirect is never followed", got)
			}
			if len(c.evil.requests()) != 0 {
				t.Error("the redirect target received a request")
			}
			if rows := auditDetails(t, c.home, "call"); !slices.Equal(rows, []string{`{"method":"GET","status":"302"}`}) {
				t.Errorf("call audit = %v, want the real status", rows)
			}
		})
	}
}

func TestCallAuditsTheLastStatusWhenInterruptedWhileWaiting(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(status(429, `{"error":"rate"}`, "Retry-After", "5"))
	ctx, cancel := context.WithCancel(context.Background())
	c.deps.sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }

	r := c.runContext(ctx, nil, "call", "-a", "work", "GET", "/x")

	c.wantError(r, ExitInternal, "interrupted")
	if rows := auditDetails(t, c.home, "call"); !slices.Equal(rows, []string{`{"method":"GET","status":"429"}`}) {
		t.Errorf("call audit = %v, want the status of the last answer", rows)
	}
}

func gzipped(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(text))
	_ = zw.Close()
	return buf.Bytes()
}
