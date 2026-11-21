package cli

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/vault"
)

var driveScopes = []string{scopeOpenID, scopeEmail, scopeDrive}

func TestTwoAppsUnderOneAliasAreTwoIndependentAccounts(t *testing.T) {
	h := newHarness(t)
	h.addClient()

	h.connect("work", "gmail")
	h.connect("work", "google-drive")

	rows := h.wantAccounts(2)
	byApp := map[string]accountRow{}
	for _, row := range rows {
		byApp[row.Provider] = row
	}
	if !containsAll(byApp["gmail"].Scopes, gmailScopes...) || slices.Contains(byApp["gmail"].Scopes, scopeDrive) || !slices.Equal(byApp["google-drive"].Scopes, driveScopes) {
		t.Errorf("gmail scopes %v, drive scopes %v; each account holds only its own app's", byApp["gmail"].Scopes, byApp["google-drive"].Scopes)
	}
	if got := strings.Fields(h.google.authorizes[0].Get("scope")); slices.Contains(got, scopeDrive) {
		t.Errorf("the gmail consent asked for %v, which includes Drive", got)
	}
	if got := strings.Fields(h.google.authorizes[1].Get("scope")); !slices.Equal(got, driveScopes) {
		t.Errorf("the drive consent asked for %v, want only %v", got, driveScopes)
	}
	for i, q := range h.google.authorizes {
		if _, widens := q["include_granted_scopes"]; widens {
			t.Errorf("authorize request %d carries include_granted_scopes", i)
		}
	}
}

func TestEachAppBindsItsAliasToItsOwnSubject(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.connect("work", "gmail")

	h.google.subject, h.google.identity = "2002", "other@work.example"
	h.connect("work", "google-drive")

	if rows := h.wantAccounts(2); rows[0].Identity == rows[1].Identity {
		t.Errorf("rows %+v; the two apps should hold different identities under one alias", rows)
	}
	h.wantError(h.run("connect", "gmail", "--as", "work"), ExitUsage, "identity-mismatch")
	h.google.subject, h.google.identity = workSubject, workIdentity
	h.wantError(h.run("connect", "google-drive", "--as", "work"), ExitUsage, "identity-mismatch")
}

func TestAccountRefsTakeAnAppIDOrToolkitCommand(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.connect("work", "google-drive")

	for _, ref := range []string{"google-drive:work", "drive:work"} {
		if show := decodeJSON[accountRow](t, h.mustRun("accounts", "show", ref)); show.Provider != "google-drive" {
			t.Errorf("show %s = %+v", ref, show)
		}
	}
	h.wantError(h.run("accounts", "show", "gmail:work"), ExitNeedsConnect, "not-connected")
}

func TestAccountsReportWhetherTheAppsScopesAreAllGranted(t *testing.T) {
	type readiness struct {
		Provider      string   `json:"provider"`
		ScopesReady   bool     `json:"scopes_ready"`
		MissingScopes []string `json:"missing_scopes"`
	}
	h := newHarness(t)
	h.addClient()
	h.connect("work", "gmail")
	h.google.grant = func(requested []string) []string {
		return slices.DeleteFunc(slices.Clone(requested), func(s string) bool { return s == scopeDrive })
	}
	h.wantError(h.run("connect", "google-drive", "--as", "work"), ExitNeedsConnect, "scopes-not-granted")

	for _, args := range [][]string{{"accounts", "list"}, {"accounts", "check"}} {
		byApp := map[string]readiness{}
		for _, row := range decodeJSON[[]readiness](t, h.mustRun(args...)) {
			byApp[row.Provider] = row
		}
		if g := byApp["gmail"]; !g.ScopesReady || g.MissingScopes == nil || len(g.MissingScopes) != 0 {
			t.Errorf("%v: gmail = %+v, want ready with an empty list", args, g)
		}
		if d := byApp["google-drive"]; d.ScopesReady || !slices.Equal(d.MissingScopes, []string{scopeDrive}) {
			t.Errorf("%v: google-drive = %+v, want not ready, missing the drive scope", args, d)
		}
	}
}

func TestAuthCheckIsAHiddenAliasOfAccountsCheck(t *testing.T) {
	c := newCallHarness(t)
	c.attachApp("google-drive", "work", workSubject)

	for _, extra := range [][]string{nil, {"--live"}, {"gmail:work"}} {
		viaAccounts := c.mustRun(append([]string{"accounts", "check"}, extra...)...)
		viaAuth := c.mustRun(append([]string{"auth", "check"}, extra...)...)
		if viaAccounts.stdout != viaAuth.stdout || viaAccounts.stderr != viaAuth.stderr {
			t.Errorf("%v: auth check printed %q / %q, accounts check %q / %q", extra, viaAuth.stdout, viaAuth.stderr, viaAccounts.stdout, viaAccounts.stderr)
		}
	}
	if help := c.mustRun("--help").stdout; regexp.MustCompile(`(?m)^\s+auth\s`).MatchString(help) || !strings.Contains(help, "accounts") {
		t.Errorf("root help lists the deprecated alias or omits accounts:\n%s", help)
	}
	if help := c.mustRun("accounts", "--help").stdout; !strings.Contains(help, "check") {
		t.Errorf("accounts help lacks check:\n%s", help)
	}
}

func TestLegacyIdentityRootAccountsAskForAReconnect(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	wire := &recordingTransport{base: http.DefaultTransport}
	h.deps.httpClient = &http.Client{Transport: wire, Timeout: 5 * time.Second}
	seedLegacyAccountWithExpiredCredential(t, h)

	for _, args := range [][]string{
		{"accounts", "show", "google:work"}, {"accounts", "check", "google:work"}, {"accounts", "check", "google:work", "--live"},
		{"accounts", "default", "google:work"}, {"call", "google", "-a", "work", "GET", "/x"},
	} {
		env := h.wantError(h.run(args...), ExitNeedsConnect, "legacy-account")
		if !strings.Contains(env.Error.Message, "addition connect gmail --as work") || !strings.Contains(env.Error.Message, "addition accounts remove google:work") {
			t.Errorf("%v: message %q does not say how to reconnect and clean up", args, env.Error.Message)
		}
	}
	live := decodeJSON[[]struct {
		Provider   string `json:"provider"`
		Connected  bool   `json:"connected"`
		Status     string `json:"status"`
		Live       *bool  `json:"live"`
		LiveReason string `json:"live_reason"`
	}](t, h.mustRun("accounts", "check", "--live"))
	if len(live) != 1 || live[0].Provider != "google" || !live[0].Connected || live[0].Status != "legacy" ||
		live[0].Live == nil || *live[0].Live || live[0].LiveReason != "legacy-account" {
		t.Errorf("bulk live check = %+v, want the legacy row, connected, not live, reason legacy-account", live)
	}
	if wire.total() != 0 {
		t.Errorf("%d requests left addition; a legacy credential must never be refreshed or sent anywhere", wire.total())
	}
	rows := h.wantAccounts(1)
	if rows[0].Provider != "google" || rows[0].Status != "legacy" {
		t.Errorf("the legacy row = %+v, want it listed with status legacy", rows[0])
	}
	if out := decodeJSON[removed](t, h.mustRun("accounts", "remove", "google:work")); !out.Removed {
		t.Errorf("remove = %+v", out)
	}
	h.wantAccounts(0)
	h.wantError(h.run("call", "google", "-a", "work", "GET", "/x"), ExitUsage, "not-an-app")
}

// seedLegacyAccountWithExpiredCredential stores google:work as an older addition did, with a real
// encrypted credential whose access token has expired, so any refresh would reach the token endpoint.
func seedLegacyAccountWithExpiredCredential(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	s, err := vault.Open(ctx, h.deps.env)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	acct, err := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work", Identity: workIdentity, Subject: workSubject, Scopes: gmailScopes})
	if err != nil {
		t.Fatal(err)
	}
	expired := h.clock.Now().Add(-time.Hour)
	if err := s.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: fakeAccessToken, RefreshToken: fakeRefreshToken, TokenType: "Bearer", ExpiresAt: expired}); err != nil {
		t.Fatal(err)
	}
}

func TestCallOnAnIdentityRootIsNotAnApp(t *testing.T) {
	c := newCallHarness(t)

	env := c.wantError(c.run("call", "google", "-a", "work", "GET", "/x"), ExitUsage, "not-an-app")

	if !strings.Contains(env.Error.Message, "google-drive") || len(c.api.requests()) != 0 {
		t.Errorf("message %q, %d requests", env.Error.Message, len(c.api.requests()))
	}
}

func TestCallNamesTheAppByIDOrToolkitCommand(t *testing.T) {
	c := newCallHarness(t)
	drive := c.attachApp("google-drive", "work", workSubject)

	for _, app := range []string{"drive", "google-drive"} {
		c.mustRun("call", app, "-a", "work", "GET", "/drive/v3/files")
	}
	c.mustRun("call", "gmail", "-a", "work", "GET", "/gmail/v1/users/me/profile")

	if n := len(drive.requests()); n != 2 {
		t.Errorf("the drive API got %d requests, want 2", n)
	}
	if n := len(c.api.requests()); n != 1 {
		t.Errorf("the gmail API got %d requests, want 1", n)
	}
}

func TestAnAppAccountCannotReachAnotherAppsHosts(t *testing.T) {
	c := newCallHarness(t, distinctTokensPerApp)
	chat := c.attachApp("google-chat", "work", workSubject)
	c.expireToken()

	r := c.run("call", "gmail", "-a", "work", "GET", chat.url("/v1/spaces"))

	c.wantError(r, ExitUsage, "host-not-allowed")
	if n := len(chat.requests()); n != 0 {
		t.Errorf("the chat server received %d requests from a gmail account", n)
	}
	if c.transport.total() != 0 {
		t.Errorf("%d requests left addition; a refused target must not even refresh the token", c.transport.total())
	}
}

func TestAChatTokenIsOnlySentToChatHosts(t *testing.T) {
	c := newCallHarness(t, distinctTokensPerApp)
	chat := c.attachApp("google-chat", "work", workSubject)

	c.mustRun("call", "google-chat", "-a", "work", "GET", "/v1/spaces")
	for _, target := range []string{c.api.url("/x"), c.evil.url("/x"), "https://gmail.googleapis.com/x"} {
		c.wantError(c.run("call", "google-chat", "-a", "work", "GET", target), ExitUsage, "host-not-allowed")
	}

	reqs := chat.requests()
	if len(reqs) != 1 || reqs[0].Header.Get("Authorization") != "Bearer "+fakeAccessToken+"-chat" {
		t.Errorf("chat requests = %+v, want one carrying the chat token", reqs)
	}
	if n := len(c.api.requests()) + len(c.evil.requests()); n != 0 {
		t.Errorf("%d requests reached the other servers", n)
	}
}

func TestEachAppRefreshesWithItsOwnRefreshTokenAndTheRootsClient(t *testing.T) {
	c := newCallHarness(t, distinctTokensPerApp)
	drive := c.attachApp("google-drive", "work", workSubject)
	c.expireToken()

	c.mustRun("call", "drive", "-a", "work", "GET", "/drive/v3/files")
	if n := c.google.refreshCount(); n != 1 {
		t.Fatalf("refresh grants = %d, want 1 (only the called app's token is refreshed)", n)
	}
	c.mustRun("call", "gmail", "-a", "work", "GET", "/x")

	wantRefresh := []string{fakeRefreshToken + "-drive", fakeRefreshToken + "-gmail"}
	for i, want := range wantRefresh {
		form := c.google.refreshForm(i)
		if form.Get("refresh_token") != want || form.Get("client_id") != fakeClientID {
			t.Errorf("refresh %d sent token %q for client %q, want %q for the Google root's client", i, form.Get("refresh_token"), form.Get("client_id"), want)
		}
	}
	if got := drive.requests(); len(got) != 1 || got[0].Header.Get("Authorization") != "Bearer "+refreshedAccessPrefix+"0001" {
		t.Errorf("drive requests = %+v, want one with the access token drive's refresh produced", got)
	}
	if got := c.api.requests(); len(got) != 1 || got[0].Header.Get("Authorization") != "Bearer "+refreshedAccessPrefix+"0002" {
		t.Errorf("gmail requests = %+v, want one with the access token gmail's refresh produced", got)
	}
}
