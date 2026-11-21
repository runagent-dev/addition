package cli

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// chunkedWithMalformedTrailer answers over a hijacked connection with a chunked body whose trailer
// line repeats the request's Authorization value, so Go's parser error would carry the token.
func chunkedWithMalformedTrailer(code int) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 %d Test\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n", code)
		_, _ = fmt.Fprintf(rw, "5\r\nhello\r\n0\r\n%s\r\n\r\n", r.Header.Get("Authorization"))
		_ = rw.Flush()
	}
}

func TestCallReportsAMalformedTrailerWithoutRepeatingIt(t *testing.T) {
	cases := []struct {
		name   string
		status int
		output bool
	}{
		{"error body", 400, false},
		{"server error body", 503, false},
		{"stdout body", 200, false},
		{"output file", 200, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCallHarness(t)
			c.api.respond(chunkedWithMalformedTrailer(tc.status))
			dir := t.TempDir()
			args := []string{"-a", "work", "GET", "/x"}
			if tc.output {
				args = append(args, "-o", filepath.Join(dir, "out.txt"))
			}

			r := c.call(args...)

			env := c.wantError(r, ExitUpstreamServer, "network")
			for _, leaked := range []string{fakeAccessToken, "Bearer", "malformed"} {
				if strings.Contains(r.stdout, leaked) || strings.Contains(r.stderr, leaked) || strings.Contains(env.Error.Message, leaked) {
					t.Errorf("%q reached the output:\nstdout: %q\nstderr: %q", leaked, r.stdout, r.stderr)
				}
			}
			if names := dirNames(t, dir); len(names) != 0 {
				t.Errorf("directory = %v, want no file", names)
			}
			assertNoTokenOnDisk(t, dir, c.home)
			assertAuditHoldsNoSecrets(t, c.harness)
		})
	}
}

func TestSealCallErrorKeepsOnlyAddnsOwnErrors(t *testing.T) {
	own := &Error{Code: "bad-output", Exit: ExitUsage, Message: "cannot write"}
	if got := sealCallError(context.Background(), own); got != own {
		t.Errorf("a typed error was replaced by %v", got)
	}

	got := asError(sealCallError(context.Background(), fmt.Errorf("parser said %s", fakeAccessToken)))

	if strings.Contains(got.Message, fakeAccessToken) || got.Code != "network" || got.Exit != ExitUpstreamServer {
		t.Errorf("failure = %+v, want the fixed network error without upstream text", got)
	}
}
