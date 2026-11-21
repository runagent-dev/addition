package cli

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reflectAuthorization answers with a body that repeats the request's Authorization header.
func reflectAuthorization(code int, contentType string) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, "the caller sent %s ok", r.Header.Get("Authorization"))
	}
}

func (c *callHarness) wantEchoRefused(r result) {
	c.t.Helper()
	env := c.wantError(r, ExitUpstreamServer, "credential-echo")
	if strings.Contains(env.Error.Message, "the caller sent") || strings.Contains(env.Error.Message, "HTTP") {
		c.t.Errorf("message %q repeats upstream text", env.Error.Message)
	}
	for _, leaked := range []string{fakeAccessToken, "ya29."} {
		if strings.Contains(r.stdout, leaked) || strings.Contains(r.stderr, leaked) {
			c.t.Errorf("%q reached the output:\nstdout: %q\nstderr: %q", leaked, r.stdout, r.stderr)
		}
	}
}

func TestCallRefusesASuccessBodyThatRepeatsTheAccessToken(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(reflectAuthorization(200, "text/plain"))

	r := c.call("-a", "work", "GET", "/x")

	c.wantEchoRefused(r)
}

func TestCallRefusesAnErrorBodyThatRepeatsTheAccessToken(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(reflectAuthorization(400, "text/plain"))

	r := c.call("-a", "work", "GET", "/x")

	c.wantEchoRefused(r)
}

func TestCallRefusesATokenSplitAcrossResponseChunks(t *testing.T) {
	c := newCallHarness(t)
	half := len(fakeAccessToken) / 2
	c.api.respond(func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("start " + fakeAccessToken[:half]))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(fakeAccessToken[half:] + " end"))
	})

	r := c.call("-a", "work", "GET", "/x")

	c.wantEchoRefused(r)
}

func TestCallRefusesATokenSplitAcrossTheErrorBodyCap(t *testing.T) {
	c := newCallHarness(t)
	atCap := maxErrorBody - len(fakeAccessToken)/2
	c.api.respond(func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(500)
		_, _ = w.Write([]byte(strings.Repeat("x", atCap) + fakeAccessToken))
	})

	r := c.call("-a", "work", "GET", "/x")

	c.wantEchoRefused(r)
}

func TestCallRefusesAnOutputFileBodyThatRepeatsTheAccessToken(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(reflectAuthorization(200, "application/octet-stream"))
	dir := t.TempDir()

	r := c.call("-a", "work", "GET", "/x", "-o", filepath.Join(dir, "out.bin"))

	c.wantEchoRefused(r)
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("directory = %v, want no output file and no temp file", names)
	}
	assertNoTokenOnDisk(t, dir, c.home)
	assertAuditHoldsNoSecrets(t, c.harness)
}

func TestCallRefusesAContentTypeThatRepeatsTheAccessToken(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(func(_ int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf; name="+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		_, _ = w.Write([]byte("%PDF-1.7\x00"))
	})
	dir := t.TempDir()

	r := c.call("-a", "work", "GET", "/x", "-o", filepath.Join(dir, "out.pdf"))

	c.wantEchoRefused(r)
	if names := dirNames(t, dir); len(names) != 0 {
		t.Errorf("directory = %v, want it empty", names)
	}
}

func TestCallRefusesABodyThatRepeatsATokenRenewedMidCall(t *testing.T) {
	c := newCallHarness(t)
	renewed := refreshedAccessPrefix + "0001"
	c.api.respond(func(n int, w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+renewed {
			status(401, `{"error":{"code":401}}`, "Content-Type", "application/json")(n, w, r)
			return
		}
		reflectAuthorization(200, "text/plain")(n, w, r)
	})

	r := c.call("-a", "work", "GET", "/x")

	env := c.wantError(r, ExitUpstreamServer, "credential-echo")
	if strings.Contains(r.stdout, renewed) || strings.Contains(r.stderr, renewed) || env.Error.Message == "" {
		t.Errorf("the renewed token reached the output:\nstdout: %q\nstderr: %q", r.stdout, r.stderr)
	}
}

func TestCallRefusesABodyThatRepeatsTheRejectedToken(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(401)
		_, _ = fmt.Fprintf(w, "%s was rejected", fakeAccessToken)
	})

	r := c.call("-a", "work", "GET", "/x")

	c.wantError(r, ExitUpstreamServer, "credential-echo")
}

func TestCallLetsABodyThatMerelyResemblesATokenThrough(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(jsonBody(`{"prefix":"ya29.","note":"` + fakeAccessToken[:len(fakeAccessToken)-1] + `"}`))

	r := c.call("-a", "work", "GET", "/x")

	if r.code != ExitOK {
		t.Errorf("exit %d (%s); a near-miss is not an echo", r.code, r.stderr)
	}
}

// assertNoTokenOnDisk walks the given directories for any file holding an access token.
func assertNoTokenOnDisk(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), fakeAccessToken) {
				t.Errorf("%s holds the access token in plaintext", path)
			}
			return nil
		})
	}
}
