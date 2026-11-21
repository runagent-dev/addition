package cli

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func auditDetails(t *testing.T, home, event string) []string {
	t.Helper()
	rows, err := openRawDB(t, home).Query(`SELECT details FROM audit_log WHERE event = ? ORDER BY id`, event)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		_ = rows.Scan(&d)
		out = append(out, d)
	}
	return out
}

func TestCallInjectsTheTokenAndStreamsTheBodyVerbatim(t *testing.T) {
	c := newCallHarness(t)
	const body = "{\n  \"files\": [{\"id\": \"1\"}]\n}\n"
	c.api.respond(jsonBody(body))

	r := c.call("-a", "work", "GET", "/drive/v3/files", "--query", "pageSize=5", "--query", "q=name contains 'x'")

	if r.code != ExitOK || r.stdout != body || r.stderr != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want the upstream body untouched", r.code, r.stdout, r.stderr)
	}
	reqs := c.api.requests()
	if len(reqs) != 1 {
		t.Fatalf("API requests = %d, want 1", len(reqs))
	}
	got := reqs[0]
	query, _ := url.ParseQuery(got.RawQuery)
	if got.Method != "GET" || got.Path != "/drive/v3/files" || query.Get("pageSize") != "5" || query.Get("q") != "name contains 'x'" {
		t.Errorf("request = %+v", got)
	}
	if got.Header.Get("Authorization") != "Bearer "+fakeAccessToken {
		t.Errorf("Authorization = %q, want the stored access token", got.Header.Get("Authorization"))
	}
	if !strings.HasPrefix(got.Header.Get("User-Agent"), "addn/") {
		t.Errorf("User-Agent = %q", got.Header.Get("User-Agent"))
	}
	if events := auditEvents(t, c.home); !slices.Equal(events, []string{"init", "connect", "call"}) {
		t.Errorf("audit events = %v", events)
	}
	if rows := auditDetails(t, c.home, "call"); !slices.Equal(rows, []string{`{"method":"GET","status":"200"}`}) {
		t.Errorf("call audit details = %v, want only the method and final status", rows)
	}
	assertAuditHoldsNoSecrets(t, c.harness)
}

func TestCallMergesThePathsQueryWithQueryFlags(t *testing.T) {
	c := newCallHarness(t)

	c.mustRun("call", "-a", "work", "GET", "/calendar/v3/calendars/primary/events?maxResults=5", "--query", "timeMin=2026-01-01T00:00:00Z")

	query, _ := url.ParseQuery(c.api.requests()[0].RawQuery)
	if query.Get("maxResults") != "5" || query.Get("timeMin") != "2026-01-01T00:00:00Z" {
		t.Errorf("query = %v", query)
	}
}

func TestCallSendsTheBodyFromAnInlineFlagAFileOrStdin(t *testing.T) {
	file := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(file, []byte(`{"from":"file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		args  []string
		stdin string
		want  string
	}{
		{"inline", []string{"--data", `{"from":"flag"}`}, "", `{"from":"flag"}`},
		{"file", []string{"--data", "@" + file}, "", `{"from":"file"}`},
		{"stdin", []string{"--data", "-"}, `{"from":"stdin"}`, `{"from":"stdin"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCallHarness(t)
			args := append([]string{"-a", "work", "POST", "/gmail/v1/users/me/labels"}, tc.args...)

			r := c.callWithStdin(tc.stdin, args...)

			if r.code != ExitOK {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			got := c.api.requests()[0]
			if got.Method != "POST" || got.Body != tc.want || got.Header.Get("Content-Type") != "application/json" {
				t.Errorf("request = %+v, want body %q as application/json", got, tc.want)
			}
		})
	}
}

func TestCallPassesOrdinaryHeadersAndKeepsAnExplicitContentType(t *testing.T) {
	c := newCallHarness(t)

	c.mustRun("call", "-a", "work", "PATCH", "/drive/v3/files/1", "--data", `{"name":"n"}`,
		"-H", "Content-Type: application/merge-patch+json", "-H", "X-Goog-User-Project: proj", "-H", "X-Multi: a", "-H", "X-Multi: b")

	h := c.api.requests()[0].Header
	if h.Get("Content-Type") != "application/merge-patch+json" || h.Get("X-Goog-User-Project") != "proj" || !slices.Equal(h.Values("X-Multi"), []string{"a", "b"}) {
		t.Errorf("headers = %v", h)
	}
}

func TestCallHandlesHeadDeleteAndEmptyBodies(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(status(204, ""))

	for _, args := range [][]string{
		{"HEAD", "/drive/v3/files/1"}, {"delete", "/drive/v3/files/1"}, {"GET", "/x"},
	} {
		r := c.call(append([]string{"-a", "work"}, args...)...)
		if r.code != ExitOK || r.stdout != "" || r.stderr != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want silence and success", args, r.code, r.stdout, r.stderr)
		}
	}
	if methods := []string{c.api.requests()[0].Method, c.api.requests()[1].Method}; !slices.Equal(methods, []string{"HEAD", "DELETE"}) {
		t.Errorf("methods = %v; a lowercase method must be upper-cased", methods)
	}
}

func TestCallAcceptsTextualBodiesWithoutOutputFile(t *testing.T) {
	for name, headers := range map[string][]string{
		"json":         {"Content-Type", "application/json"},
		"json charset": {"Content-Type", "application/json; charset=utf-8"},
		"vendor json":  {"Content-Type", "application/vnd.api+json"},
		"text":         {"Content-Type", "text/plain"},
		"csv":          {"Content-Type", "text/csv"},
		"xml":          {"Content-Type", "application/xml"},
		"no type":      {"X-Other", "x"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCallHarness(t)
			c.api.respond(status(200, "a,b\n1,2\n", headers...))

			r := c.call("-a", "work", "GET", "/x")

			if r.code != ExitOK || r.stdout != "a,b\n1,2\n" {
				t.Errorf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
			}
		})
	}
}

func TestCallRefusesBinaryBodiesWithoutOutputFile(t *testing.T) {
	for name, tc := range map[string]struct{ headers []string }{
		"pdf":          {[]string{"Content-Type", "application/pdf"}},
		"octet stream": {[]string{"Content-Type", "application/octet-stream"}},
		"image":        {[]string{"Content-Type", "image/png"}},
		"sniffed":      {[]string{"X-Other", "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCallHarness(t)
			c.api.respond(status(200, "%PDF-1.7\x00\x01\x02binary", tc.headers...))

			r := c.call("-a", "work", "GET", "/drive/v3/files/1?alt=media")

			env := c.wantError(r, ExitUsage, "binary-response")
			if r.stdout != "" || !strings.Contains(env.Error.Message, "-o") {
				t.Errorf("stdout %q, message %q; want nothing written and a hint to use -o", r.stdout, env.Error.Message)
			}
			if rows := auditDetails(t, c.home, "call"); len(rows) != 1 || rows[0] != `{"method":"GET","status":"200"}` {
				t.Errorf("call audit rows = %v; the request happened and is recorded", rows)
			}
		})
	}
}

func TestCallUpstreamFailuresKeepTheBodyOnStdoutAndTheStatusOnStderr(t *testing.T) {
	cases := []struct {
		status int
		exit   int
	}{{400, ExitUpstreamClient}, {403, ExitUpstreamClient}, {404, ExitUpstreamClient}, {500, ExitUpstreamServer}, {503, ExitUpstreamServer}}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			c := newCallHarness(t)
			const body = `{"error":{"code":1,"message":"File not found: xyz.","status":"NOT_FOUND"}}`
			c.api.respond(status(tc.status, body, "Content-Type", "application/json"))

			r := c.call("-a", "work", "GET", "/drive/v3/files/xyz")

			c.wantUpstream(r, tc.exit, tc.status, body)
			if rows := auditDetails(t, c.home, "call"); !slices.Equal(rows, []string{`{"method":"GET","status":"` + strconv.Itoa(tc.status) + `"}`}) {
				t.Errorf("call audit = %v", rows)
			}
		})
	}
}

func TestCallCapsAnErrorBodyAtOneMebibyte(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(status(500, strings.Repeat("e", 3<<20), "Content-Type", "text/plain"))

	r := c.call("-a", "work", "GET", "/x")

	if r.code != ExitUpstreamServer || len(r.stdout) != 1<<20 {
		t.Errorf("exit %d, stdout %d bytes, want 5 and exactly 1 MiB", r.code, len(r.stdout))
	}
}

func TestCallDoesNotFollowARedirect(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(status(302, "moved", "Location", c.evil.url("/landing?token=LOCATIONSECRET")))

	r := c.call("-a", "work", "GET", "/x")

	env := c.wantError(r, ExitUpstreamClient, "upstream-error")
	if env.Error.Message != "HTTP 302" || r.stdout != "" {
		t.Errorf("message %q, stdout %q; want just the status and no body from a redirect", env.Error.Message, r.stdout)
	}
	if n := len(c.evil.requests()); n != 0 {
		t.Errorf("the redirect target received %d requests", n)
	}
	if strings.Contains(r.stderr, "LOCATIONSECRET") || strings.Contains(r.stderr, c.evil.host()) {
		t.Errorf("stderr repeats the Location: %s", r.stderr)
	}
}

func TestCallNetworkFailureIsLocallyAuthored(t *testing.T) {
	c := newCallHarness(t)
	c.api.srv.Close()

	r := c.call("-a", "work", "GET", "/secret/path", "--query", "access=QUERYSECRET", "-H", "X-Secret: HEADERSECRET")

	env := c.wantError(r, ExitUpstreamServer, "network")
	for _, leak := range []string{"QUERYSECRET", "HEADERSECRET", "secret/path", "127.0.0.1", fakeAccessToken} {
		if strings.Contains(r.stderr, leak) {
			t.Errorf("the error repeats %q: %s", leak, env.Error.Message)
		}
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q", r.stdout)
	}
	if rows := auditDetails(t, c.home, "call"); !slices.Equal(rows, []string{`{"method":"GET","status":"0"}`}) {
		t.Errorf("call audit = %v, want one row with no status", rows)
	}
}

func TestCallUsageErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "missing.json")
	cases := map[string]struct {
		args []string
		code string
	}{
		"no arguments":     {nil, "usage"},
		"method only":      {[]string{"GET"}, "usage"},
		"too many":         {[]string{"GET", "/x", "/y"}, "usage"},
		"unknown method":   {[]string{"FETCH", "/x"}, "bad-method"},
		"bad query flag":   {[]string{"GET", "/x", "--query", "novalue"}, "bad-query"},
		"malformed header": {[]string{"GET", "/x", "-H", "NoColon"}, "bad-header"},
		"missing file":     {[]string{"POST", "/x", "--data", "@" + file}, "bad-data"},
		"empty file name":  {[]string{"POST", "/x", "--data", "@"}, "bad-data"},
		"relative path":    {[]string{"GET", "drive/v3/files"}, "bad-path"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newCallHarness(t)

			r := c.call(append([]string{"-a", "work"}, tc.args...)...)

			c.wantError(r, ExitUsage, tc.code)
			if r.stdout != "" || len(c.api.requests()) != 0 {
				t.Errorf("stdout %q, %d API requests; want nothing sent", r.stdout, len(c.api.requests()))
			}
		})
	}
}

func TestCallRefusesAnOversizedBody(t *testing.T) {
	old := maxRequestBody
	maxRequestBody = 16
	t.Cleanup(func() { maxRequestBody = old })
	c := newCallHarness(t)
	file := filepath.Join(t.TempDir(), "big.json")
	_ = os.WriteFile(file, []byte(strings.Repeat("x", 17)), 0o600)

	for name, r := range map[string]result{
		"stdin": c.callWithStdin(strings.Repeat("x", 17), "-a", "work", "POST", "/x", "--data", "-"),
		"file":  c.call("-a", "work", "POST", "/x", "--data", "@"+file),
	} {
		c.wantError(r, ExitUsage, "body-too-large")
		if len(c.api.requests()) != 0 {
			t.Errorf("%s: the request was sent", name)
		}
	}
}

func TestCallDecodesAsJSONWhenTheUpstreamIsJSON(t *testing.T) {
	c := newCallHarness(t)
	r := c.mustRun("call", "-a", "work", "GET", "/x")

	var v map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || v["ok"] != true {
		t.Errorf("stdout = %q, %v", r.stdout, err)
	}
}

func TestCallAnEmptyBodyIsNeverBinary(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(status(200, "", "Content-Type", "application/pdf"))

	for _, method := range []string{"HEAD", "GET"} {
		if r := c.call("-a", "work", method, "/drive/v3/files/1?alt=media"); r.code != ExitOK || r.stdout != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", method, r.code, r.stdout, r.stderr)
		}
	}
}

func TestCallSniffsABodyWithoutAContentType(t *testing.T) {
	untyped := func(body string) func(int, http.ResponseWriter, *http.Request) {
		return func(_ int, w http.ResponseWriter, _ *http.Request) {
			w.Header()["Content-Type"] = nil
			_, _ = w.Write([]byte(body))
		}
	}
	c := newCallHarness(t)

	c.api.respond(untyped("plain words"))
	if r := c.call("-a", "work", "GET", "/x"); r.code != ExitOK || r.stdout != "plain words" {
		t.Errorf("text: exit %d, stdout %q", r.code, r.stdout)
	}
	c.api.respond(untyped("PK\x03\x04\x00\x00binary"))
	c.wantError(c.call("-a", "work", "GET", "/x"), ExitUsage, "binary-response")
}

func TestCallReportsABodyThatBreaksMidStreamAsANetworkError(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write([]byte(`{"partial":`))
	})

	r := c.call("-a", "work", "GET", "/x")

	c.wantError(r, ExitUpstreamServer, "network")
}
