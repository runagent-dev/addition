package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// Every secret the harness knows is checked, at cleanup, against the stdout and stderr of every
// command below; the audit log and vault are checked here.
func TestCallNeverLeaksCredentialsOnAnyPath(t *testing.T) {
	c := newCallHarness(t, func(g *fakeGoogle) { g.rotateRefresh = true })
	dir := t.TempDir()
	run := func(args ...string) { c.call(append([]string{"-a", "work"}, args...)...) }

	run("GET", "/x")
	c.expireToken()
	run("GET", "/x", "--query", "k=v")
	c.api.respond(requireToken("never-matches"))
	run("GET", "/x")
	c.api.respond(status(429, `{"error":"rate"}`))
	run("GET", "/x")
	c.api.respond(status(302, "", "Location", c.evil.url("/x")))
	run("GET", "/x")
	c.api.respond(status(503, `{"error":"down"}`))
	run("POST", "/x", "--data", `{"a":1}`)
	c.api.respond(status(200, "%PDF\x00", "Content-Type", "application/pdf"))
	run("GET", "/x")
	run("GET", "/x", "-o", filepath.Join(dir, "a.pdf"))
	run("GET", "/x", "-o", filepath.Join(dir, "a.pdf"))
	run("GET", c.evil.url("/x"))
	run("GET", "/x", "-H", "Authorization: Bearer x")
	run("GET", "/x", "-H", "Cookie: c")
	c.google.configure(func(g *fakeGoogle) { g.refreshError = "invalid_grant" })
	c.expireToken()
	run("GET", "/x")
	run("GET", "/x")
	c.mustRun("accounts", "check", "--live")
	c.mustRun("accounts", "list")
	c.mustRun("accounts", "show", "gmail:work")
	c.run("doctor")
	c.run("doctor", "--live")
	c.api.srv.Close()
	run("GET", "/x")
	c.run("doctor", "--live")

	assertAuditHoldsNoSecrets(t, c.harness)
	for _, row := range auditDetails(t, c.home, "call") {
		if strings.ContainsAny(row, "/?") {
			t.Errorf("a call audit row holds a path or query: %s", row)
		}
	}
}

func TestCallNeverEchoesUpstreamHeadersIntoTheEnvelope(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(status(400, `{"error":"bad"}`, "Content-Type", "application/json", "X-Echo", fakeAccessToken, "WWW-Authenticate", `Bearer error="invalid_token", realm="`+echoMarker+`"`))

	r := c.call("-a", "work", "GET", "/x")

	c.wantUpstream(r, ExitUpstreamClient, 400, `{"error":"bad"}`)
	if strings.Contains(r.stderr, echoMarker) || strings.Contains(r.stderr, fakeAccessToken) {
		t.Errorf("stderr repeats upstream headers: %s", r.stderr)
	}
}
