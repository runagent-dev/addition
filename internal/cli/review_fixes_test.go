package cli

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/vault"
)

func TestConnectFlagErrorsNeverRepeatWhatWasTyped(t *testing.T) {
	const typed = "pk_typed_secret_value"
	cases := map[string]struct {
		args    []string
		message string
	}{
		"a value on the bool --token-stdin, with =": {[]string{"connect", "gmail", "--as", "w", "--token-stdin=" + typed}, "--token-stdin"},
		"a value on the bool --no-browser":          {[]string{"connect", "gmail", "--as", "w", "--no-browser=" + typed}, "--no-browser"},
		"an unknown flag named like a secret":       {[]string{"connect", "gmail", "--as", "w", "--" + typed}, ""},
		"an unknown flag given a value":             {[]string{"connect", "gmail", "--as", "w", "--" + typed + "=" + typed}, ""},
		"an unknown shorthand run into its value":   {[]string{"connect", "gmail", "--as", "w", "-p" + typed}, ""},
		"bad flag syntax":                           {[]string{"connect", "gmail", "--as", "w", "---" + typed}, ""},
		"a flag with no value":                      {[]string{"connect", "gmail", "--as", "w", "--token-env"}, "--token-env"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := run(tc.args...)

			if r.code != ExitUsage {
				t.Fatalf("exit %d, want a usage error\nstderr: %s", r.code, r.stderr)
			}
			env := decodeError(t, lastLine(r.stderr))
			if env.Error.Code != "usage" || env.Error.Message == "" {
				t.Errorf("envelope = %+v", env)
			}
			assertNoLeak(t, r, typed)
			if !strings.Contains(env.Error.Message, tc.message) {
				t.Errorf("message = %q, want it to name %q", env.Error.Message, tc.message)
			}
		})
	}
}

func TestConnectNeverEchoesAnAppNameThatMayBeAToken(t *testing.T) {
	const typed = "pk_typed_secret_value"

	r := run("connect", typed, "--as", "w", "--token-stdin")

	if r.code != ExitUsage || decodeError(t, lastLine(r.stderr)).Error.Code != "unknown-provider" {
		t.Fatalf("exit %d, stderr %s; want unknown-provider", r.code, r.stderr)
	}
	assertNoLeak(t, r, typed)
}

func newDualHarness(t *testing.T) *tokenHarness {
	t.Helper()
	th := newTokenHarness(t)
	th.deps.catalogFS = withProviderFile(t, th.deps.catalogFS, "dualapp",
		dualAppYAML(th.api.host(), strings.TrimPrefix(th.google.srv.URL, "http://"), th.google.srv.URL))
	return th
}

func (th *tokenHarness) sawHost(host string) bool {
	th.transport.mu.Lock()
	defer th.transport.mu.Unlock()
	return slices.Contains(th.transport.seen, host)
}

type tokenLiveRow struct {
	Live       *bool  `json:"live"`
	LiveReason string `json:"live_reason"`
}

func TestALiveCheckNeverSendsATokenToTheOAuthIdentityURLAfterTheCredentialBecameAToken(t *testing.T) {
	th := newDualHarness(t)
	th.seedAccount("dualapp", "me", "1", th.oauthCredential(true))
	swapped := false
	th.deps.beforeLiveToken = func() {
		if !swapped {
			swapped = true
			th.replaceCredential("dualapp", "me", vault.Credential{Kind: vault.KindAPIKey, AccessToken: pastedToken})
		}
	}

	rows := decodeJSON[[]tokenLiveRow](t, th.mustRun("accounts", "check", "--live"))

	if th.sawHost(strings.TrimPrefix(th.google.srv.URL, "http://")) {
		t.Error("a request reached the OAuth identity URL with a credential that had become a token")
	}
	if len(rows) != 1 || rows[0].Live == nil || !*rows[0].Live {
		t.Errorf("rows = %+v, want the token verified at the app's own endpoint", rows)
	}
	reqs := th.api.requests()
	if len(reqs) != 1 || reqs[0].Path != "/v1/me" || reqs[0].Header.Get("Authorization") != "Bearer "+pastedToken {
		t.Errorf("API requests = %+v, want one verification with the token's header", reqs)
	}
}

func TestALiveCheckNeverSendsAnOAuthTokenToTheAPIAfterTheCredentialBecameOAuth(t *testing.T) {
	th := newDualHarness(t)
	th.seedAccount("dualapp", "me", "1", vault.Credential{Kind: vault.KindAPIKey, AccessToken: pastedToken})
	swapped := false
	th.deps.beforeLiveToken = func() {
		if !swapped {
			swapped = true
			th.replaceCredential("dualapp", "me", th.oauthCredential(false))
		}
	}

	rows := decodeJSON[[]tokenLiveRow](t, th.mustRun("accounts", "check", "--live"))

	if n := len(th.api.requests()); n != 0 {
		t.Errorf("the API saw %d requests carrying an OAuth token with a token's header", n)
	}
	if len(rows) != 1 || rows[0].Live == nil || !*rows[0].Live {
		t.Errorf("rows = %+v, want the OAuth identity endpoint to vouch for it", rows)
	}
	if !th.sawHost(strings.TrimPrefix(th.google.srv.URL, "http://")) {
		t.Error("the OAuth identity URL was never asked")
	}
}

const grantSubject = "S-1"

func grantProvidersYAML(googleURL string) map[string]string {
	root := "id: \"grantroot\"\nname: \"Grant Root\"\nauth_type: \"oauth2\"\n" +
		"oauth2:\n  authorize_url: \"" + googleURL + "/authorize\"\n  token_url: \"" + googleURL + "/token\"\n  revoke_url: \"" + googleURL + "/revoke\"\n" +
		"  revoke_scope: \"grant\"\n  refresh_supported: true\n  pkce_supported: true\n  pkce_required: false\n  default_scopes: []\n" +
		"  scopes_delimiter: \"space\"\n  authorize_params: {}\n  token_params: {}\n  token_response_format: \"json\"\n  client_auth_method: \"body\"\n" +
		"api:\n  base_url: \"https://api.grant.example\"\n  auth_header: \"Authorization\"\n  auth_prefix: \"Bearer\"\n"
	return map[string]string{
		"grantroot":   root,
		"grantroot-a": "id: \"grantroot-a\"\nname: \"Grant A\"\nauth_type: \"oauth2\"\nextends: \"grantroot\"\n",
		"grantroot-b": "id: \"grantroot-b\"\nname: \"Grant B\"\nauth_type: \"api_key\"\nextends: \"grantroot\"\n" +
			"api_key:\n  create_url: \"https://grant.example/tokens\"\n  identity:\n    path: \"/me\"\n    field: \"login\"\n    subject_field: \"id\"\n",
	}
}

func newGrantHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	for id, body := range grantProvidersYAML(h.google.srv.URL) {
		h.deps.catalogFS = withProviderFile(t, h.deps.catalogFS, id, body)
	}
	h.secrets = append(h.secrets, pastedToken)
	return h
}

func (h *harness) statusOf(ref string) string {
	h.t.Helper()
	provider, alias, _ := strings.Cut(ref, ":")
	for _, row := range decodeJSON[[]tokenAccountRow](h.t, h.mustRun("accounts", "list")) {
		if row.Provider == provider && row.Alias == alias {
			return row.Status
		}
	}
	return "absent"
}

func tokenCredential() vault.Credential {
	return vault.Credential{Kind: vault.KindAPIKey, AccessToken: pastedToken}
}

func TestRevokeIgnoresTokenAccountsOfTheSameSubject(t *testing.T) {
	h := newGrantHarness(t)
	h.seedAccount("grantroot-a", "x", grantSubject, h.oauthCredential(true))
	h.seedAccount("grantroot-b", "tok", grantSubject, tokenCredential())

	got := decodeJSON[struct {
		Revoked         bool     `json:"revoked"`
		AlsoInvalidated []string `json:"also_invalidated"`
	}](t, h.mustRun("accounts", "remove", "grantroot-a:x", "--revoke"))

	if !got.Revoked || len(got.AlsoInvalidated) != 0 {
		t.Errorf("result = %+v, want the OAuth grant revoked and nothing else touched", got)
	}
	if status := h.statusOf("grantroot-b:tok"); status != "active" {
		t.Errorf("the token account is %q; revoking an OAuth grant does not reach it", status)
	}
}

func TestOnlyOAuthSiblingsAreNamedAndInvalidated(t *testing.T) {
	h := newGrantHarness(t)
	h.seedAccount("grantroot-a", "x", grantSubject, h.oauthCredential(true))
	h.seedAccount("grantroot-a", "other", grantSubject, h.oauthCredential(true))
	h.seedAccount("grantroot-b", "tok", grantSubject, tokenCredential())

	env := h.wantError(h.run("accounts", "remove", "grantroot-a:x", "--revoke"), ExitUsage, "revoke-affects-other-apps")
	if !strings.Contains(env.Error.Message, "grantroot-a:other") || strings.Contains(env.Error.Message, "grantroot-b:tok") {
		t.Errorf("message = %q, want only the OAuth sibling named", env.Error.Message)
	}

	got := decodeJSON[struct {
		AlsoInvalidated []string `json:"also_invalidated"`
	}](t, h.mustRun("accounts", "remove", "grantroot-a:x", "--revoke", "--and-siblings"))

	if !slices.Equal(got.AlsoInvalidated, []string{"grantroot-a:other"}) {
		t.Errorf("also_invalidated = %v", got.AlsoInvalidated)
	}
	if h.statusOf("grantroot-a:other") != "needs-reauth" || h.statusOf("grantroot-b:tok") != "active" {
		t.Error("the OAuth sibling must need reauth and the token account must stay active")
	}
}

func TestASiblingThatBecameATokenBeforeTheRemovalCommitsIsNotMarked(t *testing.T) {
	h := newGrantHarness(t)
	h.seedAccount("grantroot-a", "x", grantSubject, h.oauthCredential(true))
	h.seedAccount("grantroot-a", "other", grantSubject, h.oauthCredential(true))
	swapped := false
	h.deps.beforeRemoveCommit = func() {
		if !swapped {
			swapped = true
			h.replaceCredential("grantroot-a", "other", tokenCredential())
		}
	}

	got := decodeJSON[struct {
		AlsoInvalidated []string `json:"also_invalidated"`
	}](t, h.mustRun("accounts", "remove", "grantroot-a:x", "--revoke", "--and-siblings"))

	if len(got.AlsoInvalidated) != 0 || h.statusOf("grantroot-a:other") != "active" {
		t.Errorf("also_invalidated = %v, status %q; a token account the revocation cannot reach must stay active", got.AlsoInvalidated, h.statusOf("grantroot-a:other"))
	}
}

func seededGitHub(t *testing.T, th *tokenHarness, cred vault.Credential) {
	t.Helper()
	th.deps.catalogFS = editProvider(t, th.deps.catalogFS, "github", func(data []byte) []byte {
		data = mustReplace(t, data, "    - \"api.github.com\"", "    - \"api.github.com\"\n    - \""+th.api.host()+"\"")
		return mustReplace(t, data, `base_url: "https://api.github.com"`, `base_url: "https://`+th.api.host()+`"`)
	})
	scopes := []string{}
	if cred.Kind != vault.KindAPIKey {
		scopes = []string{"repo", "read:user", "user:email"}
	}
	th.seedAccountWithScopes("github", "me", "583231", scopes, cred)
}

func TestDoctorAsksForAClientOnlyForRootsWithOAuthCredentials(t *testing.T) {
	const fixGitHub = "addition client add github --file client_secret.json"

	t.Run("a GitHub OAuth account without a client", func(t *testing.T) {
		th := newTokenHarness(t)
		seededGitHub(t, th, th.oauthCredential(true))

		rep := wantNeedsAction(t, th.run("doctor"), fixGitHub)

		if c := rep.check(t, "oauth-client:github"); c.Status != "warn" || c.Fix != fixGitHub {
			t.Errorf("check = %+v", c)
		}
		if rep.has("oauth-client:google") {
			t.Error("a vault with no Google account was asked for a Google client")
		}
	})
	t.Run("a GitHub OAuth account with a client", func(t *testing.T) {
		th := newTokenHarness(t)
		seededGitHub(t, th, th.oauthCredential(true))
		th.mustRun("client", "add", "github", "--file", writeClientFile(t, "installed"))

		rep := wantHealthy(t, th.run("doctor"))

		if c := rep.check(t, "oauth-client:github"); c.Status != "ok" {
			t.Errorf("check = %+v", c)
		}
	})
	t.Run("a GitHub token account needs none", func(t *testing.T) {
		th := newTokenHarness(t)
		seededGitHub(t, th, tokenCredential())

		rep := wantHealthy(t, th.run("doctor"))

		for _, c := range rep.Checks {
			if strings.HasPrefix(c.ID, "oauth-client:") {
				t.Errorf("check %+v for a vault of token accounts", c)
			}
		}
	})
	t.Run("Google OAuth beside a token account", func(t *testing.T) {
		c := newCallHarness(t)
		c.secrets = append(c.secrets, pastedToken)
		c.deps.catalogFS = withProviderFile(t, c.deps.catalogFS, tokenApp, tokenProviderYAML(c.evil.host(), tokenProviderOpts{}))
		c.evil.respond(requireTokenBody(pastedToken, `{"id":4242,"login":"octo"}`))
		if r := c.runWithStdin(strings.NewReader(pastedToken+"\n"), "connect", tokenApp, "--as", "w", "--token-stdin"); r.code != ExitOK {
			t.Fatalf("connect: %d %s", r.code, r.stderr)
		}

		rep := wantHealthy(t, c.run("doctor"))

		if c := rep.check(t, "oauth-client:google"); c.Status != "ok" {
			t.Errorf("check = %+v", c)
		}
		if rep.has("oauth-client:" + tokenApp) {
			t.Error("a token app was asked for an OAuth client")
		}
		rawExec(t, c.home, `DELETE FROM oauth_clients`)
		wantNeedsAction(t, c.run("doctor"), fixAddClient)
	})
}

func TestReconnectHintsNameTheOAuthMethodWhenTheAppDefaultsToATokenButTheAccountIsOAuth(t *testing.T) {
	th := newTokenHarness(t)
	seededGitHub(t, th, th.oauthCredential(false))

	env := th.wantError(th.run("call", "github", "GET", "/user"), ExitNeedsConnect, "needs-reauth")
	if !strings.Contains(env.Error.Message, "addition connect github --as me --method oauth2") {
		t.Errorf("message = %q, want the command that connects with OAuth", env.Error.Message)
	}

	rep := wantNeedsAction(t, th.run("doctor"), "addition connect github --as me --method oauth2")
	if c := rep.check(t, "account:github:me"); c.Fix != "addition connect github --as me --method oauth2" {
		t.Errorf("account check = %+v", c)
	}
}

func TestATokenHintNeverNeedsTheMethodFlagBecauseTheTokenFlagImpliesIt(t *testing.T) {
	th := newTokenHarness(t)
	seededGitHub(t, th, tokenCredential())
	th.accept(secondToken)

	env := th.wantError(th.run("call", "github", "GET", "/user"), ExitNeedsConnect, "needs-reauth")

	if !strings.Contains(env.Error.Message, "addition connect github --as me --token-stdin") || strings.Contains(env.Error.Message, "--method") {
		t.Errorf("message = %q", env.Error.Message)
	}
}

func TestATokenCheckIsASingleAttempt(t *testing.T) {
	th := newTokenHarness(t)
	th.api.respond(status(http.StatusTooManyRequests, `{}`, "Content-Type", "application/json", "Retry-After", "0"))

	r := th.connectToken("work", pastedToken)

	th.wantError(r, ExitUpstreamClient, "identity-rejected")
	if n := len(th.api.requests()); n != 1 {
		t.Errorf("the API saw %d requests for one token check; a 429 must not be retried", n)
	}
	th.wantNothingStored()
}

func TestALiveTokenCheckIsASingleAttempt(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)
	th.api.respond(status(http.StatusTooManyRequests, `{}`, "Content-Type", "application/json", "Retry-After", "0"))

	rows := decodeJSON[[]tokenLiveRow](t, th.mustRun("accounts", "check", "--live"))

	if n := len(th.api.requests()); n != 1 || rows[0].LiveReason != "identity-rejected" {
		t.Errorf("requests = %d, rows = %+v; want one attempt", n, rows)
	}
}

func TestATokenCheckThatStallsAfterTheHeadersEndsAtItsDeadline(t *testing.T) {
	th := newTokenHarness(t)
	old := verifyTimeout
	verifyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { verifyTimeout = old })
	th.api.respond(func(_ int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	r := th.runContext(ctx, strings.NewReader(pastedToken+"\n"), "connect", tokenApp, "--as", "work", "--token-stdin")

	th.wantError(r, ExitUpstreamServer, "network")
	th.wantNothingStored()
}
