package cli

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

type connected struct {
	Provider string   `json:"provider"`
	Alias    string   `json:"alias"`
	Identity string   `json:"identity"`
	Scopes   []string `json:"scopes"`
	Created  bool     `json:"created"`
}

type pendingOut struct {
	AuthorizeURL string `json:"authorize_url"`
	ExpiresAt    string `json:"expires_at"`
	Next         string `json:"next"`
}

var gmailScopes = []string{scopeOpenID, scopeEmail, scopeGmailRO}

func containsAll(have []string, want ...string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

func TestConnectLoopbackFlow(t *testing.T) {
	h := newHarness(t)
	h.addClient()

	r := h.mustRun("connect", "google", "--as", "work", "--services", "gmail")

	got := decodeJSON[connected](t, r)
	if got.Provider != "google" || got.Alias != "work" || got.Identity != workIdentity || !got.Created {
		t.Errorf("connect = %+v", got)
	}
	if !containsAll(got.Scopes, gmailScopes...) || slices.Contains(got.Scopes, scopeDrive) {
		t.Errorf("scopes = %v, want gmail's", got.Scopes)
	}
	auth := h.google.authorizes[0]
	if auth.Get("access_type") != "offline" || auth.Get("prompt") != "consent" || auth.Get("code_challenge_method") != "S256" {
		t.Errorf("authorize params = %v", auth)
	}
	redirect, _ := url.Parse(auth.Get("redirect_uri"))
	if redirect.Hostname() != "127.0.0.1" || redirect.Path != "/callback" || redirect.Port() == "" {
		t.Errorf("redirect_uri = %s, want a loopback /callback", redirect)
	}
	if h.google.pkceOK != 1 {
		t.Errorf("the verifier sent at exchange did not match the challenge (%d good exchanges)", h.google.pkceOK)
	}
	if rows := auditEvents(t, h.home); !slices.Equal(rows, []string{"init", "connect"}) {
		t.Errorf("audit events = %v", rows)
	}
	h.assertNothingLeaked()
	assertAuditHoldsNoSecrets(t, h)
}

func TestConnectPrintsTheURLWhenTheBrowserFailsToOpen(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.deps.openBrowser = func(authorizeURL string) error {
		go func() { _ = h.browse(authorizeURL) }()
		return errNoBrowser
	}

	r := h.mustRun("connect", "google", "--as", "work", "--services", "gmail")

	if !strings.Contains(r.stderr, h.google.srv.URL+"/authorize?") {
		t.Errorf("stderr does not carry the authorize URL: %q", r.stderr)
	}
	if got := decodeJSON[connected](t, r); got.Identity != workIdentity {
		t.Errorf("connect = %+v", got)
	}
	h.assertNothingLeaked()
}

func TestConnectLoopbackTimesOut(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.deps.openBrowser = func(string) error { return nil }
	h.deps.loopbackTimeout = 20 * time.Millisecond

	h.wantError(h.run("connect", "google", "--as", "work"), ExitUsage, "authorization-timeout")
	h.wantAccounts(0)
}

func TestConnectLoopbackRejectsAStateMismatch(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.deps.openBrowser = func(authorizeURL string) error {
		u, _ := url.Parse(authorizeURL)
		back := u.Query().Get("redirect_uri") + "?code=4/forged&state=not-the-state"
		resp, err := http.Get(back)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}

	h.wantError(h.run("connect", "google", "--as", "work"), ExitUsage, "state-mismatch")
	if len(h.google.exchanges) != 0 {
		t.Error("a forged callback reached the token endpoint")
	}
	h.wantAccounts(0)
}

func (h *harness) startTwoStep(args ...string) pendingOut {
	h.t.Helper()
	r := h.mustRun(append([]string{"connect", "google", "--as", "work", "--no-browser"}, args...)...)
	return decodeJSON[pendingOut](h.t, r)
}

func TestTwoStepFlow(t *testing.T) {
	h := newHarness(t)
	h.addClient()

	p := h.startTwoStep("--services", "gmail,drive")
	if !strings.HasPrefix(p.AuthorizeURL, h.google.srv.URL+"/authorize?") {
		t.Errorf("authorize_url = %q", p.AuthorizeURL)
	}
	if !strings.HasPrefix(p.Next, "addn connect --complete ") {
		t.Errorf("next = %q", p.Next)
	}
	if exp, err := time.Parse(time.RFC3339, p.ExpiresAt); err != nil || !exp.Equal(h.clock.Now().Add(10*time.Minute)) {
		t.Errorf("expires_at = %q, want 10 minutes from now", p.ExpiresAt)
	}
	redirect, _ := url.Parse(mustQueryParam(t, p.AuthorizeURL, "redirect_uri"))
	if redirect.Hostname() != "127.0.0.1" || redirect.Path != "/callback" {
		t.Errorf("redirect_uri = %s", redirect)
	}

	landed := h.google.consent(p.AuthorizeURL)
	r := h.mustRun("connect", "--complete", landed)

	got := decodeJSON[connected](t, r)
	if got.Alias != "work" || got.Identity != workIdentity || !got.Created || !containsAll(got.Scopes, append(gmailScopes, scopeDrive)...) {
		t.Errorf("complete = %+v", got)
	}
	if h.google.pkceOK != 1 {
		t.Error("the stored verifier did not match the challenge in the authorize URL")
	}
	for _, v := range h.google.verifiers {
		if strings.Contains(dumpDB(t, h.home), v) {
			t.Error("the PKCE verifier is in the database in plaintext")
		}
	}
	h.assertNothingLeaked()
	assertAuditHoldsNoSecrets(t, h)
}

func mustQueryParam(t *testing.T, raw, key string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get(key)
}

func TestCompleteIsOneUse(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	landed := h.google.consent(h.startTwoStep().AuthorizeURL)
	h.mustRun("connect", "--complete", landed)

	h.wantError(h.run("connect", "--complete", landed), ExitUsage, "unknown-state")
	if len(h.google.exchanges) != 1 {
		t.Errorf("token endpoint hit %d times, want once", len(h.google.exchanges))
	}
}

func TestCompleteRejectsAnExpiredAuthorization(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	landed := h.google.consent(h.startTwoStep().AuthorizeURL)
	h.clock.Advance(10*time.Minute + time.Second)

	h.wantError(h.run("connect", "--complete", landed), ExitUsage, "authorization-expired")
	h.clock.Advance(-time.Hour)
	h.wantError(h.run("connect", "--complete", landed), ExitUsage, "unknown-state")
	if len(h.google.exchanges) != 0 {
		t.Error("an expired authorization reached the token endpoint")
	}
}

func TestCompleteRejectsBadPastes(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	landed := h.google.consent(h.startTwoStep().AuthorizeURL)
	u, _ := url.Parse(landed)

	forged := *u
	q := forged.Query()
	q.Set("state", "never-issued")
	forged.RawQuery = q.Encode()

	moved := *u
	moved.Host = "127.0.0.1:1"

	h.wantError(h.run("connect", "--complete", "not a url"), ExitUsage, "bad-redirect")
	h.wantError(h.run("connect", "--complete", forged.String()), ExitUsage, "unknown-state")
	h.wantError(h.run("connect", "--complete", moved.String()), ExitUsage, "redirect-mismatch")
	h.wantError(h.run("connect", "google", "--complete", landed), ExitUsage, "usage")
	if len(h.google.exchanges) != 0 {
		t.Error("a bad paste reached the token endpoint")
	}
}

func TestCompleteAccessDenied(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.google.deny = true
	landed := h.google.consent(h.startTwoStep().AuthorizeURL)

	h.wantError(h.run("connect", "--complete", landed), ExitUpstreamClient, "access-denied")
	h.wantError(h.run("connect", "--complete", landed), ExitUsage, "unknown-state")
	h.wantAccounts(0)
}

func TestConnectTokenExchangeRejected(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.google.tokenStatus = http.StatusBadRequest
	landed := h.google.consent(h.startTwoStep().AuthorizeURL)

	env := h.wantError(h.run("connect", "--complete", landed), ExitUpstreamClient, "token-exchange-rejected")
	if !strings.Contains(env.Error.Message, "invalid_grant") {
		t.Errorf("message %q does not name the provider error", env.Error.Message)
	}
	h.wantAccounts(0)
	h.assertNothingLeaked()
}

func TestConnectNetworkFailure(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	landed := h.google.consent(h.startTwoStep().AuthorizeURL)
	h.google.srv.Close()

	h.wantError(h.run("connect", "--complete", landed), ExitUpstreamServer, "network")
	h.wantAccounts(0)
}

func TestConnectScopesNotGranted(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.google.grant = func(requested []string) []string {
		return slices.DeleteFunc(slices.Clone(requested), func(s string) bool { return s == scopeDrive })
	}

	r := h.run("connect", "google", "--as", "work", "--services", "gmail,drive")

	env := h.wantError(r, ExitNeedsConnect, "scopes-not-granted")
	if !strings.Contains(env.Error.Message, scopeDrive) {
		t.Errorf("message %q does not list the missing scope", env.Error.Message)
	}
	got := decodeJSON[connected](t, r)
	if slices.Contains(got.Scopes, scopeDrive) || !containsAll(got.Scopes, gmailScopes...) {
		t.Errorf("saved scopes = %v, want only the granted ones", got.Scopes)
	}
	show := decodeJSON[accountRow](t, h.mustRun("accounts", "show", "google:work"))
	if !slices.Equal(show.Scopes, got.Scopes) {
		t.Errorf("stored scopes = %v, want %v", show.Scopes, got.Scopes)
	}
	h.assertNothingLeaked()
}

func TestReconnectRequestsTheScopeUnion(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.mustRun("connect", "google", "--as", "work", "--services", "gmail")

	r := h.mustRun("connect", "google", "--as", "work", "--services", "drive")

	requested := strings.Fields(h.google.authorizes[1].Get("scope"))
	if !containsAll(requested, append(gmailScopes, scopeDrive)...) {
		t.Errorf("reconnect requested %v, want gmail's granted scopes plus drive", requested)
	}
	got := decodeJSON[connected](t, r)
	if got.Created || !containsAll(got.Scopes, append(gmailScopes, scopeDrive)...) {
		t.Errorf("reconnect = %+v", got)
	}
	h.wantAccounts(1)
	if rows := auditEvents(t, h.home); !slices.Equal(rows, []string{"init", "connect", "connect"}) {
		t.Errorf("audit events = %v", rows)
	}
	h.assertNothingLeaked()
}

func TestReconnectRefusesADifferentIdentity(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.mustRun("connect", "google", "--as", "work", "--services", "gmail")
	h.google.identity = "someone-else@example.com"

	h.wantError(h.run("connect", "google", "--as", "work", "--services", "drive"), ExitUsage, "identity-mismatch")
	show := decodeJSON[accountRow](t, h.mustRun("accounts", "show", "google:work"))
	if show.Identity != workIdentity || slices.Contains(show.Scopes, scopeDrive) {
		t.Errorf("the account changed: %+v", show)
	}
}

func TestConnectServiceResolution(t *testing.T) {
	h := newHarness(t)
	h.addClient()

	for _, name := range []string{"drive", "google-drive"} {
		scopes := strings.Fields(mustQueryParam(t, h.startTwoStep("--services", name).AuthorizeURL, "scope"))
		if !containsAll(scopes, scopeOpenID, scopeEmail, scopeDrive) || slices.Contains(scopes, scopeGmailRO) {
			t.Errorf("--services %s requested %v", name, scopes)
		}
	}

	all := strings.Fields(mustQueryParam(t, h.startTwoStep().AuthorizeURL, "scope"))
	if !containsAll(all, scopeOpenID, scopeEmail, scopeProfile, scopeGmailRO, scopeDrive, scopeCalendar) {
		t.Errorf("default services requested %v, want the root plus every toolkit service", all)
	}
	if len(all) != len(slices.Compact(slices.Sorted(slices.Values(all)))) {
		t.Errorf("scopes are not deduplicated: %v", all)
	}

	h.wantError(h.run("connect", "google", "--as", "work", "--services", "nope"), ExitUsage, "unknown-service")
	h.wantError(h.run("connect", "google", "--as", "work", "--services", "gmail,slack"), ExitUsage, "service-not-in-provider")
}

func TestConnectUsageErrors(t *testing.T) {
	h := newHarness(t)
	env := h.wantError(h.run("connect", "google", "--as", "work"), ExitUsage, "no-oauth-client")
	if !strings.Contains(env.Error.Message, "addn client add") {
		t.Errorf("message %q does not point to addn client add", env.Error.Message)
	}
	h.addClient()
	h.wantError(h.run("connect", "nope", "--as", "work"), ExitUsage, "unknown-provider")
	h.wantError(h.run("connect", "gmail", "--as", "work"), ExitUsage, "not-identity-root")
	h.wantError(h.run("connect", "google"), ExitUsage, "usage")
	h.wantError(h.run("connect", "google", "--as", "Not An Alias!"), ExitUsage, "invalid-account")
	h.wantError(h.run("connect"), ExitUsage, "usage")
}

var errNoBrowser = errors.New("no browser here")
