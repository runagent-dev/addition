package cli

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCallRefreshesAnExpiredTokenBeforeSending(t *testing.T) {
	c := newCallHarness(t)
	c.expireToken()

	c.mustRun("call", "gmail", "-a", "work", "GET", "/x")

	if c.google.refreshCount() != 1 {
		t.Fatalf("refresh grants = %d, want 1", c.google.refreshCount())
	}
	form := c.google.refreshForm(0)
	if form.Get("refresh_token") != fakeRefreshToken || form.Get("client_id") != fakeClientID || form.Get("client_secret") != fakeClientSecret {
		t.Errorf("refresh request = %v", form)
	}
	if got := c.api.requests()[0].Header.Get("Authorization"); got != "Bearer "+refreshedAccessPrefix+"0001" {
		t.Errorf("Authorization = %q, want the refreshed token", got)
	}
	if events := auditEvents(t, c.home); !slices.Equal(events, []string{"init", "connect", "refresh", "call"}) {
		t.Errorf("audit events = %v", events)
	}
	if rows := auditDetails(t, c.home, "refresh"); !slices.Equal(rows, []string{`{"outcome":"ok"}`}) {
		t.Errorf("refresh audit = %v", rows)
	}
	c.mustRun("call", "gmail", "-a", "work", "GET", "/x")
	if c.google.refreshCount() != 1 {
		t.Error("the refreshed token was refreshed again before it expired")
	}
	assertAuditHoldsNoSecrets(t, c.harness)
}

func TestCallStoresARotatedRefreshToken(t *testing.T) {
	c := newCallHarness(t, func(g *fakeGoogle) { g.rotateRefresh = true })
	c.expireToken()
	c.mustRun("call", "gmail", "-a", "work", "GET", "/x")
	c.expireToken()

	c.mustRun("call", "gmail", "-a", "work", "GET", "/x")

	if got := c.google.refreshForm(1).Get("refresh_token"); got != rotatedRefreshPrefix+"0001" {
		t.Errorf("the second refresh sent %q, want the rotated refresh token", got)
	}
	assertAuditHoldsNoSecrets(t, c.harness)
}

func TestConcurrentCallsRefreshTheTokenOnce(t *testing.T) {
	c := newCallHarness(t)
	c.expireToken()
	const callers = 6
	results := make([]result, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = c.call("-a", "work", "GET", "/x")
		}()
	}
	wg.Wait()

	for i, r := range results {
		if r.code != ExitOK {
			t.Errorf("caller %d: exit %d: %s", i, r.code, r.stderr)
		}
	}
	if n := c.google.refreshCount(); n != 1 {
		t.Errorf("token endpoint refresh grants = %d, want exactly 1", n)
	}
	for _, req := range c.api.requests() {
		if req.Header.Get("Authorization") != "Bearer "+refreshedAccessPrefix+"0001" {
			t.Errorf("a call went out with %q", req.Header.Get("Authorization"))
		}
	}
}

func TestInvalidGrantMeansNeedsReauthAndNeverLoops(t *testing.T) {
	c := newCallHarness(t, func(g *fakeGoogle) { g.refreshError = "invalid_grant" })
	c.expireToken()

	r := c.call("-a", "work", "GET", "/x")

	env := c.wantError(r, ExitNeedsConnect, "needs-reauth")
	if !strings.Contains(env.Error.Message, "addn connect google --as work") {
		t.Errorf("message %q does not tell the user to reconnect", env.Error.Message)
	}
	if r.stdout != "" || len(c.api.requests()) != 0 {
		t.Errorf("stdout %q, %d API requests; want nothing sent", r.stdout, len(c.api.requests()))
	}
	if rows := c.wantAccounts(1); rows[0].Status != "needs-reauth" {
		t.Errorf("account status = %q, want needs-reauth", rows[0].Status)
	}
	if rows := auditDetails(t, c.home, "refresh"); !slices.Equal(rows, []string{`{"outcome":"needs-reauth"}`}) {
		t.Errorf("refresh audit = %v", rows)
	}
	c.call("-a", "work", "GET", "/x")
	if n := c.google.refreshCount(); n != 1 {
		t.Errorf("refresh grants = %d, want 1: a dead refresh token is never retried", n)
	}

	c.google.configure(func(g *fakeGoogle) { g.refreshError = "" })
	c.mustRun("connect", "gmail", "--as", "work")
	c.mustRun("call", "gmail", "-a", "work", "GET", "/x")
}

func TestOtherRefreshFailuresAreUpstreamErrorsAndKeepTheAccount(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
		exit   int
	}{
		{"server error", 500, "server_error", ExitUpstreamServer},
		{"client rejected", 401, "invalid_client", ExitUpstreamClient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCallHarness(t, func(g *fakeGoogle) { g.refreshError, g.refreshStatus = tc.code, tc.status })
			c.expireToken()

			r := c.call("-a", "work", "GET", "/x")

			c.wantError(r, tc.exit, "refresh-rejected")
			if rows := c.wantAccounts(1); rows[0].Status != "active" {
				t.Errorf("status = %q, want active", rows[0].Status)
			}
			if len(c.api.requests()) != 0 {
				t.Error("a request was sent without a usable token")
			}
		})
	}
}

func TestA401IsRefreshedAndRetriedOnce(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(requireToken(refreshedAccessPrefix + "0001"))

	r := c.mustRun("call", "gmail", "-a", "work", "POST", "/x", "--data", `{"a":1}`)

	if n := c.google.refreshCount(); n != 1 {
		t.Errorf("refresh grants = %d, want 1", n)
	}
	reqs := c.api.requests()
	if len(reqs) != 2 || reqs[0].Header.Get("Authorization") != "Bearer "+fakeAccessToken || reqs[1].Body != `{"a":1}` {
		t.Errorf("requests = %+v, want the old token, then the new one with the body replayed", reqs)
	}
	if r.stdout != `{"ok":true}` {
		t.Errorf("stdout = %q", r.stdout)
	}
	if rows := auditDetails(t, c.home, "call"); !slices.Equal(rows, []string{`{"method":"POST","status":"200"}`}) {
		t.Errorf("call audit = %v, want the final status only", rows)
	}
}

func TestASecond401IsAnUpstreamError(t *testing.T) {
	c := newCallHarness(t)
	const body = `{"error":{"code":401}}`
	c.api.respond(status(401, body, "Content-Type", "application/json"))

	r := c.call("-a", "work", "GET", "/x")

	c.wantUpstream(r, ExitUpstreamClient, 401, body)
	if n, reqs := c.google.refreshCount(), len(c.api.requests()); n != 1 || reqs != 2 {
		t.Errorf("refresh grants %d, API requests %d; want 1 and 2", n, reqs)
	}
}

func TestA401WithoutARefreshTokenNeedsReconnect(t *testing.T) {
	c := newCallHarness(t, func(g *fakeGoogle) { g.noRefreshToken = true })
	c.api.respond(status(401, `{"error":{"code":401}}`, "Content-Type", "application/json"))

	r := c.call("-a", "work", "GET", "/x")

	env := c.wantError(r, ExitNeedsConnect, "needs-reauth")
	if !strings.Contains(env.Error.Message, "addn connect google --as work") {
		t.Errorf("message %q", env.Error.Message)
	}
	if len(c.api.requests()) != 1 || c.google.refreshCount() != 0 {
		t.Errorf("API requests %d, refresh grants %d; want 1 and 0", len(c.api.requests()), c.google.refreshCount())
	}
	if rows := auditDetails(t, c.home, "call"); !slices.Equal(rows, []string{`{"method":"GET","status":"401"}`}) {
		t.Errorf("call audit = %v", rows)
	}
}

func TestCallRetriesThrottlingHonouringRetryAfter(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(func(n int, w http.ResponseWriter, r *http.Request) {
		if n <= 2 {
			status(429, `{"error":"rate"}`, "Retry-After", "2")(n, w, r)
			return
		}
		jsonBody(`{"ok":true}`)(n, w, r)
	})

	r := c.mustRun("call", "gmail", "-a", "work", "POST", "/x", "--data", "payload")

	if got := c.slept(); !slices.Equal(got, []time.Duration{2 * time.Second, 2 * time.Second}) {
		t.Errorf("sleeps = %v, want [2s 2s]", got)
	}
	reqs := c.api.requests()
	if len(reqs) != 3 || r.stdout != `{"ok":true}` {
		t.Fatalf("API requests %d, stdout %q", len(reqs), r.stdout)
	}
	for i, req := range reqs {
		if req.Header.Get("Authorization") != "Bearer "+fakeAccessToken || req.Body != "payload" {
			t.Errorf("attempt %d = %+v, want the same credentials and body", i, req)
		}
	}
	if rows := auditDetails(t, c.home, "call"); len(rows) != 1 {
		t.Errorf("call audit rows = %v, want one per command, not per attempt", rows)
	}
}

func TestCallGivesUpAfterThreeThrottleRetries(t *testing.T) {
	c := newCallHarness(t)
	const body = `{"error":"rate"}`
	c.api.respond(status(429, body, "Content-Type", "application/json"))

	r := c.call("-a", "work", "GET", "/x")

	c.wantUpstream(r, ExitUpstreamClient, 429, body)
	if n := len(c.api.requests()); n != 4 {
		t.Errorf("API requests = %d, want 1 + 3 retries", n)
	}
	slept := c.slept()
	if len(slept) != 3 {
		t.Fatalf("sleeps = %v", slept)
	}
	for _, d := range slept {
		if d <= 0 || d > 30*time.Second {
			t.Errorf("wait %s is outside (0, 30s]", d)
		}
	}
}
