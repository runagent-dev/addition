package cli

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestCallRefusesTheCallersAcceptEncoding(t *testing.T) {
	for _, header := range []string{"Accept-Encoding: gzip", "accept-encoding: br", "Accept-Encoding: identity"} {
		c := newCallHarness(t)

		r := c.call("-a", "work", "GET", "/x", "-H", header)

		c.wantError(r, ExitUsage, "forbidden-header")
		if n := len(c.api.requests()); n != 0 {
			t.Errorf("%s: %d requests were sent, want none", header, n)
		}
	}
}

// encodedReply answers with a content coding whatever the request asked for, holding the caller's token.
func encodedReply(code int, coding string, body func(token string) []byte) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", coding)
		w.WriteHeader(code)
		_, _ = w.Write(body(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	}
}

func TestCallNeverReleasesABodyThatCarriesAContentCoding(t *testing.T) {
	const (
		undecoded = "unsupported-encoding"
		echoed    = "credential-echo"
	)
	cases := []struct {
		name    string
		coding  string
		args    []string
		status  int
		output  bool
		code    string
		exit    int
		gzipped bool
	}{
		{"br on 200", "br", nil, 200, false, undecoded, ExitUpstreamServer, false},
		{"br on 200 saved", "br", nil, 200, true, undecoded, ExitUpstreamServer, false},
		{"br on 400", "br", nil, 400, false, undecoded, ExitUpstreamClient, false},
		{"br on 503", "br", nil, 503, false, undecoded, ExitUpstreamServer, false},
		{"zstd on 200", "zstd", nil, 200, false, undecoded, ExitUpstreamServer, false},
		{"gzip with a range request on 200", "gzip", []string{"-H", "Range: bytes=0-"}, 200, false, undecoded, ExitUpstreamServer, true},
		{"gzip with a range request on 200 saved", "gzip", []string{"-H", "Range: bytes=0-"}, 200, true, undecoded, ExitUpstreamServer, true},
		{"gzip with a range request on 400", "gzip", []string{"-H", "Range: bytes=0-"}, 400, false, undecoded, ExitUpstreamClient, true},
		{"gzip the transport decodes on 200", "gzip", nil, 200, false, echoed, ExitUpstreamServer, true},
		{"gzip the transport decodes on 200 saved", "gzip", nil, 200, true, echoed, ExitUpstreamServer, true},
		{"gzip the transport decodes on 400", "gzip", nil, 400, false, echoed, ExitUpstreamServer, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCallHarness(t)
			c.api.respond(encodedReply(tc.status, tc.coding, func(token string) []byte {
				if tc.gzipped {
					return gzipped(t, `{"echo":"`+token+`"}`)
				}
				return []byte(`{"echo":"` + token + `"}`)
			}))
			dir := t.TempDir()
			args := append([]string{"-a", "work", "GET", "/x"}, tc.args...)
			if tc.output {
				args = append(args, "-o", filepath.Join(dir, "out.json"))
			}

			r := c.call(args...)

			c.wantError(r, tc.exit, tc.code)
			if r.stdout != "" {
				t.Errorf("stdout = %q, want nothing", r.stdout)
			}
			if names := dirNames(t, dir); len(names) != 0 {
				t.Errorf("directory = %v, want no file", names)
			}
			assertNoTokenOnDisk(t, dir, c.home)
			assertAuditHoldsNoSecrets(t, c.harness)
		})
	}
}

func TestCallLetsAnIdentityCodingThrough(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(status(200, `{"ok":true}`, "Content-Type", "application/json", "Content-Encoding", "identity"))

	r := c.call("-a", "work", "GET", "/x")

	if r.code != ExitOK || r.stdout != `{"ok":true}` {
		t.Errorf("exit %d, stdout %q; identity is no coding", r.code, r.stdout)
	}
}
