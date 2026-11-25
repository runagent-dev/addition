package cli

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestATokenAccountIsReadyWithoutScopesOrARefreshToken(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)

	rows := decodeJSON[[]tokenAccountRow](t, th.mustRun("accounts", "list"))
	show := decodeJSON[tokenAccountRow](t, th.mustRun("accounts", "show", tokenApp+":work"))
	check := decodeJSON[[]struct {
		tokenAccountRow
		Connected   bool   `json:"connected"`
		AccessToken string `json:"access_token"`
	}](t, th.mustRun("accounts", "check"))

	for name, row := range map[string]tokenAccountRow{"list": rows[0], "show": show, "check": check[0].tokenAccountRow} {
		if row.Method != "api_key" || !row.ScopesReady || row.MissingScopes == nil || len(row.MissingScopes) != 0 ||
			row.HasRefreshToken || row.ExpiresAt != nil || row.Status != "active" || row.Scopes == nil || len(row.Scopes) != 0 {
			t.Errorf("accounts %s = %+v", name, row)
		}
	}
	if rows[0].Identity != tokenIdentity || show.Identity != tokenIdentity {
		t.Errorf("identity = %q, %q, want %q", rows[0].Identity, show.Identity, tokenIdentity)
	}
	if !check[0].Connected || check[0].AccessToken != "no-expiry" {
		t.Errorf("check = %+v, want connected with a token that never expires", check[0])
	}
}

func TestOAuthAccountsReportTheirMethodToo(t *testing.T) {
	c := newCallHarness(t)

	rows := decodeJSON[[]tokenAccountRow](t, c.mustRun("accounts", "list"))

	if len(rows) != 1 || rows[0].Method != "oauth2" || !rows[0].HasRefreshToken {
		t.Errorf("accounts = %+v", rows)
	}
}

func TestAccountsCheckLiveVerifiesATokenAccountAtItsIdentityEndpoint(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)

	rows := decodeJSON[[]struct {
		Live       *bool  `json:"live"`
		LiveReason string `json:"live_reason"`
	}](t, th.mustRun("accounts", "check", "--live"))

	if len(rows) != 1 || rows[0].Live == nil || !*rows[0].Live || rows[0].LiveReason != "ok" {
		t.Fatalf("rows = %+v, want live ok", rows)
	}
	reqs := th.api.requests()
	if len(reqs) != 1 || reqs[0].Path != "/v1/me" || reqs[0].Header.Get("Authorization") != "Bearer "+pastedToken {
		t.Errorf("requests = %+v, want one authenticated GET of the identity endpoint", reqs)
	}
}

func TestAccountsCheckLiveReportsARevokedToken(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)
	th.accept(secondToken)

	r := th.mustRun("accounts", "check", "--live")

	rows := decodeJSON[[]struct {
		Status     string `json:"status"`
		Live       *bool  `json:"live"`
		LiveReason string `json:"live_reason"`
	}](t, r)
	if rows[0].Live == nil || *rows[0].Live || rows[0].LiveReason != "token-rejected" {
		t.Errorf("rows = %+v, want live false, token-rejected", rows)
	}
	if rows[0].Status != "active" {
		t.Errorf("status = %q: a live check reports, it does not change the account", rows[0].Status)
	}
}

func TestAccountsCheckLiveForATokenAccountNeverTouchesAnOAuthEndpoint(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)

	th.mustRun("accounts", "check", "--live")

	th.google.mu.Lock()
	defer th.google.mu.Unlock()
	if len(th.google.refreshes)+len(th.google.exchanges) != 0 {
		t.Error("a token account reached the OAuth token endpoint")
	}
}

func TestACallWithATokenAccountSendsTheTokenAndPrintsTheBody(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)

	r := th.call("-a", "work", "GET", "/v1/me", "--query", "per_page=1")

	if r.code != ExitOK {
		t.Fatalf("exit %d\nstderr: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, `"login":"octo"`) {
		t.Errorf("stdout = %q, want the API body", r.stdout)
	}
	reqs := th.api.requests()
	last := reqs[len(reqs)-1]
	if last.Header.Get("Authorization") != "Bearer "+pastedToken || last.RawQuery != "per_page=1" {
		t.Errorf("request = %+v", last)
	}
}

func TestACallWithATokenAccountRefusesHostsOutsideTheAllowList(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)

	r := th.call("GET", th.evil.url("/steal"))

	th.wantError(r, ExitUsage, "host-not-allowed")
	if n := len(th.evil.requests()); n != 0 {
		t.Errorf("the token reached another host: %d requests", n)
	}
}

func TestAResponseThatEchoesATokenAccountsTokenIsWithheld(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)
	th.api.respond(jsonBody(`{"token":"` + pastedToken + `"}`))

	r := th.call("GET", "/v1/me")

	th.wantError(r, ExitUpstreamServer, "credential-echo")
	if r.stdout != "" {
		t.Errorf("stdout = %q, want nothing", r.stdout)
	}
}

func TestA401MarksATokenAccountNeedsReauthWithoutRefreshingOrRetrying(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)
	th.accept(secondToken)

	r := th.call("GET", "/v1/me")

	env := th.wantError(r, ExitNeedsConnect, "needs-reauth")
	if !strings.Contains(env.Error.Message, "addition connect "+tokenApp+" --as work --token-stdin") {
		t.Errorf("message = %q, want the command that replaces the token", env.Error.Message)
	}
	if n := len(th.api.requests()); n != 1 {
		t.Errorf("the API saw %d requests, want exactly the one that was rejected", n)
	}
	th.google.mu.Lock()
	refreshed := len(th.google.refreshes) + len(th.google.exchanges)
	th.google.mu.Unlock()
	if refreshed != 0 {
		t.Error("an api key was sent to an OAuth token endpoint")
	}
	if rows := decodeJSON[[]tokenAccountRow](t, th.mustRun("accounts", "list")); rows[0].Status != "needs-reauth" {
		t.Errorf("status = %q, want needs-reauth", rows[0].Status)
	}

	again := th.call("GET", "/v1/me")

	envAgain := th.wantError(again, ExitNeedsConnect, "needs-reauth")
	if !strings.Contains(envAgain.Error.Message, "--token-stdin") {
		t.Errorf("message = %q, want the token command on later calls too", envAgain.Error.Message)
	}
	if n := len(th.api.requests()); n != 1 {
		t.Errorf("a later call sent a request (%d total); it must fail before sending", n)
	}
}

func TestA403IsAnUpstreamErrorNotAReauth(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)
	th.api.respond(status(http.StatusForbidden, `{"message":"no"}`, "Content-Type", "application/json"))

	r := th.call("GET", "/v1/me")

	th.wantError(r, ExitUpstreamClient, "upstream-error")
	if rows := decodeJSON[[]tokenAccountRow](t, th.mustRun("accounts", "list")); rows[0].Status != "active" {
		t.Errorf("status = %q, want active", rows[0].Status)
	}
}

func TestDoctorTreatsATokenAccountAsHealthyWithoutAnOAuthClient(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)

	rep := wantHealthy(t, th.run("doctor"))

	acct := rep.check(t, "account:"+tokenApp+":work")
	if acct.Status != "ok" || acct.Code != "ok" || acct.Fix != "" {
		t.Errorf("account check = %+v", acct)
	}
	for _, c := range rep.Checks {
		if strings.HasPrefix(c.ID, "cloud-setup:") || strings.HasPrefix(c.ID, "oauth-client:") || strings.HasSuffix(c.ID, ":token-age") {
			t.Errorf("check %+v does not apply to a vault of token accounts", c)
		}
	}
}

func TestDoctorStillAsksForAnOAuthClientWhenTheVaultHoldsOnlyOtherRoots(t *testing.T) {
	th := newTokenHarness(t)

	rep := wantNeedsAction(t, th.run("doctor"), fixAddClient)

	if !rep.has("oauth-client:google") {
		t.Error("a fresh vault lost its first-run client hint")
	}
}

func TestDoctorLiveProbesATokenAccountLikeAnyOther(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)

	rep := wantHealthy(t, th.run("doctor", "--live"))

	if c := rep.check(t, "live:"+tokenApp+":work"); c.Status != "ok" {
		t.Errorf("live check = %+v", c)
	}
	reqs := th.api.requests()
	if len(reqs) != 1 || reqs[0].Path != "/v1/me" || reqs[0].Header.Get("Authorization") != "Bearer "+pastedToken {
		t.Errorf("requests = %+v, want the probe with the token", reqs)
	}
}

func TestDoctorLiveProbesATokenAccountAtItsIdentityPathWhenTheAppHasNoProbePath(t *testing.T) {
	th := newTokenHarness(t, tokenProviderOpts{noProbe: true})
	th.mustConnectToken("work", pastedToken)

	rep := wantHealthy(t, th.run("doctor", "--live"))

	if c := rep.check(t, "live:"+tokenApp+":work"); c.Status != "ok" {
		t.Errorf("live check = %+v, want the identity endpoint to serve as the probe", c)
	}
	if reqs := th.api.requests(); len(reqs) != 1 || reqs[0].Path != "/v1/me" {
		t.Errorf("requests = %+v", reqs)
	}
}

func TestDoctorLiveNamesTheTokenCommandForARevokedToken(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)
	th.accept(secondToken)
	fix := "addition connect " + tokenApp + " --as work --token-stdin"

	rep := wantNeedsAction(t, th.run("doctor", "--live"), fix)

	c := rep.check(t, "live:"+tokenApp+":work")
	if c.Status != "fail" || c.Code != "needs-reauth" || c.Fix != fix {
		t.Errorf("live check = %+v, want a failure that names the token command", c)
	}
	if n := len(th.api.requests()); n != 1 {
		t.Errorf("the probe was sent %d times, want once", n)
	}
}

func TestDoctorNamesTheTokenCommandForAnAccountThatNeedsReauth(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)
	th.accept(secondToken)
	th.wantError(th.call("GET", "/v1/me"), ExitNeedsConnect, "needs-reauth")
	fix := "addition connect " + tokenApp + " --as work --token-stdin"

	rep := wantNeedsAction(t, th.run("doctor"), fix)

	if c := rep.check(t, "account:"+tokenApp+":work"); c.Code != "needs-reauth" || c.Fix != fix {
		t.Errorf("account check = %+v", c)
	}
}

func TestDoctorNeverCallsATokenAccountWithoutAScopeOrRefreshTokenBroken(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)

	rep := decodeDoctor(t, th.run("doctor"))

	for _, c := range rep.Checks {
		if c.Code == "no-refresh-token" || c.Code == "scopes-missing" {
			t.Errorf("check %+v blames a token account for what it never has", c)
		}
	}
}

func TestProvidersListAuthMethods(t *testing.T) {
	h := newHarness(t)

	rows := decodeJSON[[]struct {
		ID          string   `json:"id"`
		AuthType    string   `json:"auth_type"`
		AuthMethods []string `json:"auth_methods"`
		Connectable bool     `json:"connectable"`
		Extends     *string  `json:"extends"`
	}](t, h.mustRun("providers"))

	want := map[string][]string{
		"clickup": {"api_key"}, "github": {"api_key", "oauth2"}, "gmail": {"oauth2"}, "google": {"oauth2"}, "slack": {"oauth2"},
	}
	seen := 0
	for _, row := range rows {
		if methods, ok := want[row.ID]; ok {
			seen++
			if !slices.Equal(row.AuthMethods, methods) {
				t.Errorf("%s auth_methods = %v, want %v", row.ID, row.AuthMethods, methods)
			}
		}
		if row.ID == "github" && (row.AuthType != "api_key" || !row.Connectable) {
			t.Errorf("github = %+v, want api_key as the default and connectable", row)
		}
		if row.ID == "gmail" && (row.AuthType != "oauth2" || row.Extends == nil || *row.Extends != "google") {
			t.Errorf("gmail = %+v, the existing fields must stay", row)
		}
	}
	if seen != len(want) {
		t.Errorf("only %d of the expected providers were listed", seen)
	}
}

func TestRemovingATokenAccountNeverSendsItsTokenToARevokeEndpoint(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)

	r := th.mustRun("accounts", "remove", tokenApp+":work", "--revoke")

	got := decodeJSON[removed](t, r)
	if !got.Removed || got.Revoke.Attempted || got.Revoke.OK || got.Revoke.Error == "" {
		t.Errorf("result = %+v, want removed locally with a note that addition cannot revoke an API token", got)
	}
	if n := len(th.api.requests()); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
	th.wantNothingStored()
}

func TestClientAddAcceptsAProviderThatOffersOAuthBesidesATokenDefault(t *testing.T) {
	h := newHarness(t)

	r := h.run("client", "add", "github", "--file", writeClientFile(t, "installed"))

	if r.code != ExitOK {
		t.Errorf("exit %d, want 0: github has an oauth2 block, so it takes an OAuth client although its default is a token\nstderr: %s", r.code, r.stderr)
	}
}
