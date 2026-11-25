package cli

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/proxy"
	"github.com/runagent-dev/addition/internal/vault"
)

const marker = "ghp_synthetic_review_marker"

func TestRootErrorsNeverRepeatWhatWasTyped(t *testing.T) {
	cases := map[string]struct {
		args    []string
		message string
	}{
		"help for an unknown topic":                {[]string{"help", marker}, "addition --help"},
		"help for a known command and extra input": {[]string{"help", "connect", marker}, "addition --help"},
		"an unknown command":                       {[]string{marker}, "unknown command"},
		"an unknown command with flags":            {[]string{marker, "--as", "w"}, "unknown command"},
		"an unknown subcommand":                    {[]string{"accounts", marker}, "unknown command"},
		"an extra argument":                        {[]string{"version", marker}, "unknown command"},
		"a value on a bool flag":                   {[]string{"accounts", "check", "--live=" + marker}, "--live"},
		"a value on doctor's bool flag":            {[]string{"doctor", "--live=" + marker}, "--live"},
		"an unknown flag":                          {[]string{"providers", "--" + marker}, "unknown flag"},
		"an unknown flag with a value":             {[]string{"call", "--" + marker + "=" + marker}, "unknown flag"},
		"an unknown shorthand":                     {[]string{"call", "-z" + marker}, "unknown flag"},
		"a flag with no value":                     {[]string{"call", "gmail", "GET", "/x", "--query"}, "--query"},
		"an unknown guide topic":                   {[]string{"guide", marker}, ""},
		"an unknown provider for tools":            {[]string{"tools", "--provider", marker}, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := run(tc.args...)

			if r.code != ExitUsage {
				t.Fatalf("exit %d, want a usage error\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
			}
			assertNoLeak(t, r, marker)
			env := decodeError(t, lastLine(r.stderr))
			if env.Error.Message == "" || !strings.Contains(env.Error.Message, tc.message) {
				t.Errorf("message = %q, want it to say %q", env.Error.Message, tc.message)
			}
		})
	}
}

func TestVaultBackedCommandsNeverRepeatATypedAppOrAlias(t *testing.T) {
	h := newHarness(t)
	cases := map[string][]string{
		"show with no colon":             {"accounts", "show", marker},
		"show with an unknown app":       {"accounts", "show", marker + ":w"},
		"show with an unknown alias":     {"accounts", "show", "gmail:" + marker},
		"remove with an unknown alias":   {"accounts", "remove", "gmail:" + marker},
		"default with an unknown app":    {"accounts", "default", marker + ":w"},
		"check with an unknown alias":    {"accounts", "check", "gmail:" + marker},
		"call with an unknown app":       {"call", marker, "GET", "/x"},
		"call with an unknown alias":     {"call", "gmail", "-a", marker, "GET", "/x"},
		"client add with an unknown app": {"client", "add", marker, "--file", "x.json"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			r := h.run(args...)

			if r.code == ExitOK {
				t.Fatalf("exit 0 for %v", args)
			}
			assertNoLeak(t, r, marker)
		})
	}
}

func TestAnAliasThatLooksLikeACredentialIsRefusedWithoutRepeatingIt(t *testing.T) {
	h := newHarness(t)
	for name, args := range map[string][]string{
		"a token default connect": {"connect", "github", "--as", marker},
		"a token connect":         {"connect", "github", "--as", marker, "--token-stdin"},
		"an OAuth connect":        {"connect", "gmail", "--as", marker, "--no-browser"},
	} {
		t.Run(name, func(t *testing.T) {
			r := h.runWithStdin(neverReadStdin{t}, args...)

			env := h.wantError(r, ExitUsage, "invalid-account")
			assertNoLeak(t, r, marker)
			if !strings.Contains(env.Error.Message, "credential") {
				t.Errorf("message = %q, want it to say why", env.Error.Message)
			}
		})
	}
}

func TestAnAliasThatIsOrContainsTheSuppliedTokenIsRefused(t *testing.T) {
	const token = "abcd1234efgh"
	for name, alias := range map[string]string{"equal": token, "containing": "work-" + token} {
		t.Run(name, func(t *testing.T) {
			th := newTokenHarness(t, tokenProviderOpts{noPrefix: true})
			th.secrets = append(th.secrets, token)

			r := th.runWithStdin(strings.NewReader(token+"\n"), "connect", tokenApp, "--as", alias, "--token-stdin")

			th.wantError(r, ExitUsage, "invalid-account")
			if strings.Contains(r.stderr, token) {
				t.Errorf("stderr repeats the alias: %q", r.stderr)
			}
			if n := len(th.api.requests()); n != 0 {
				t.Errorf("%d requests were sent", n)
			}
			th.wantNothingStored()
		})
	}
}

func TestACallerGivenAReasonableAliasIsStillRefusedByLength(t *testing.T) {
	h := newHarness(t)

	h.wantError(h.run("connect", "gmail", "--as", strings.Repeat("a", 33), "--no-browser"), ExitUsage, "invalid-account")
}

func TestARevokeNeverRevokesAGrantThatAppearedAfterTheSiblingsWereChosen(t *testing.T) {
	h := newGrantHarness(t)
	h.seedAccount("grantroot-b", "t", grantSubject, tokenCredential())
	h.seedAccount("grantroot-a", "other", grantSubject, h.oauthCredential(true))
	swapped := false
	h.deps.afterSiblingSelection = func() {
		if !swapped {
			swapped = true
			h.replaceCredential("grantroot-b", "t", h.oauthCredential(true))
		}
	}

	h.mustRun("accounts", "remove", "grantroot-b:t", "--revoke")

	h.google.mu.Lock()
	revoked := len(h.google.revoked)
	h.google.mu.Unlock()
	if revoked != 0 {
		t.Errorf("a grant was revoked (%d requests) although its siblings were chosen from a token account, so they stay active", revoked)
	}
}

func pointGitHubAtFakes(t *testing.T, th *tokenHarness) {
	t.Helper()
	g := th.google.srv.URL
	th.deps.catalogFS = editProvider(t, th.deps.catalogFS, "github", func(data []byte) []byte {
		data = mustReplace(t, data, "    - \"api.github.com\"", "    - \"api.github.com\"\n    - \""+th.api.host()+"\"")
		data = mustReplace(t, data, `base_url: "https://api.github.com"`, `base_url: "https://`+th.api.host()+`"`)
		data = mustReplace(t, data, `authorize_url: "https://github.com/login/oauth/authorize"`, `authorize_url: "`+g+`/authorize"`)
		data = mustReplace(t, data, `token_url: "https://github.com/login/oauth/access_token"`, `token_url: "`+g+`/token"`)
		data = mustReplace(t, data, `pkce_supported: false`, `pkce_supported: true`)
		return []byte(strings.ReplaceAll(string(data), "https://api.github.com/user", "https://"+th.api.host()+"/user"))
	})
	th.accept(pastedToken, fakeAccessToken, fakeAccessToken)
	th.mustRun("client", "add", "github", "--file", writeClientFile(t, "installed"))
}

func connectGitHubOAuth(th *tokenHarness) result {
	return th.run("connect", "github", "--as", "me", "--method", "oauth2")
}

func TestGitHubOAuthAfterATokenForTheSameUserKeepsTheAlias(t *testing.T) {
	th := newTokenHarness(t)
	pointGitHubAtFakes(t, th)
	if r := th.runWithStdin(strings.NewReader(pastedToken+"\n"), "connect", "github", "--as", "me", "--token-stdin"); r.code != ExitOK {
		t.Fatalf("token connect: %d %s", r.code, r.stderr)
	}

	r := connectGitHubOAuth(th)

	if r.code != ExitOK {
		t.Fatalf("OAuth connect: exit %d\nstderr: %s", r.code, r.stderr)
	}
	if got := decodeJSON[tokenConnectResult](t, r); got.Identity != tokenIdentity || got.Method != "oauth2" {
		t.Errorf("result = %+v, want the user's login and the oauth2 method", got)
	}
}

func TestAGitHubTokenOfAnotherUserCannotTakeOverAnOAuthAlias(t *testing.T) {
	th := newTokenHarness(t)
	pointGitHubAtFakes(t, th)
	if r := connectGitHubOAuth(th); r.code != ExitOK {
		t.Fatalf("OAuth connect: exit %d\nstderr: %s", r.code, r.stderr)
	}
	th.accept(secondToken)
	th.setSubject("9999")

	r := th.runWithStdin(strings.NewReader(secondToken+"\n"), "connect", "github", "--as", "me", "--token-stdin")

	th.wantError(r, ExitUsage, "identity-mismatch")
}

func TestASubjectlessAliasKeepsItsDisplayIdentityAgainstANewToken(t *testing.T) {
	th := newTokenHarness(t)
	th.seedAccount(tokenApp, "work", "", vault.Credential{AccessToken: "old"})
	rawExec(t, th.home, `UPDATE accounts SET identity = 'someone-else'`)

	th.wantError(th.connectToken("work", pastedToken), ExitUsage, "identity-mismatch")

	rawExec(t, th.home, `UPDATE accounts SET identity = ''`)
	if r := th.connectToken("work", pastedToken); r.code != ExitOK {
		t.Fatalf("nothing stored to compare: exit %d\nstderr: %s", r.code, r.stderr)
	}
	if rows := decodeJSON[[]tokenAccountRow](t, th.mustRun("accounts", "list")); rows[0].Identity != tokenIdentity {
		t.Errorf("accounts = %+v, want the alias bound to the token's identity", rows)
	}
}

func TestALiveCheckOfATokenWithAnIdentitySchemaReadsAndGuardsTheIdentity(t *testing.T) {
	cases := map[string]struct {
		handle func(int, http.ResponseWriter, *http.Request)
		reason string
	}{
		"an HTML page":               {status(http.StatusOK, "<html>maintenance</html>", "Content-Type", "text/html"), "identity-rejected"},
		"JSON without the fields":    {jsonBody(`{"unrelated":true}`), "identity-rejected"},
		"a body repeating the token": {jsonBody(`{"id":7,"login":"` + pastedToken + `"}`), "identity-rejected"},
		"headers then a stall": {func(_ int, w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":`))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}, "network"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			th := newTokenHarness(t)
			th.mustConnectToken("work", pastedToken)
			old := verifyTimeout
			verifyTimeout = 300 * time.Millisecond
			t.Cleanup(func() { verifyTimeout = old })
			th.api.respond(tc.handle)

			rows := decodeJSON[[]tokenLiveRow](t, th.mustRun("accounts", "check", "--live"))

			if rows[0].Live == nil || *rows[0].Live || rows[0].LiveReason != tc.reason {
				t.Errorf("rows = %+v, want live false with %s", rows, tc.reason)
			}
		})
	}
}

func TestALiveCheckOfATokenWithoutAnIdentitySchemaOnlyNeedsASuccessStatus(t *testing.T) {
	th := newTokenHarness(t, tokenProviderOpts{noIdentity: true})
	th.mustConnectToken("work", pastedToken)
	th.api.respond(status(http.StatusOK, "<html>fine</html>", "Content-Type", "text/html"))

	rows := decodeJSON[[]tokenLiveRow](t, th.mustRun("accounts", "check", "--live"))

	if rows[0].Live == nil || !*rows[0].Live {
		t.Errorf("rows = %+v, want live", rows)
	}
}

func TestDoctorLiveChoosesTheProbeFromTheKindOfTheCredentialItReads(t *testing.T) {
	t.Run("OAuth replaced by a token before the read", func(t *testing.T) {
		th := newDualHarness(t)
		th.seedAccount("dualapp", "me", "1", th.oauthCredential(true))
		swapped := false
		th.deps.beforeLiveToken = func() {
			if !swapped {
				swapped = true
				th.replaceCredential("dualapp", "me", tokenCredential())
			}
		}

		rep := decodeDoctor(t, th.run("doctor", "--live"))

		if c := rep.check(t, "live:dualapp:me"); c.Status != "ok" {
			t.Errorf("live check = %+v, want the token probed at its identity path", c)
		}
		if reqs := th.api.requests(); len(reqs) != 1 || reqs[0].Header.Get("Authorization") != "Bearer "+pastedToken {
			t.Errorf("API requests = %+v", reqs)
		}
	})
	t.Run("a token replaced by OAuth before the read", func(t *testing.T) {
		th := newDualHarness(t)
		th.seedAccount("dualapp", "me", "1", tokenCredential())
		swapped := false
		th.deps.beforeLiveToken = func() {
			if !swapped {
				swapped = true
				th.replaceCredential("dualapp", "me", th.oauthCredential(true))
			}
		}

		rep := decodeDoctor(t, th.run("doctor", "--live"))

		if c := rep.check(t, "live:dualapp:me"); c.Status != "skip" {
			t.Errorf("live check = %+v, want a skip: an OAuth credential has no probe here", c)
		}
		if n := len(th.api.requests()); n != 0 {
			t.Errorf("the API saw %d requests carrying an OAuth token", n)
		}
	})
}

func TestTheAccountChangedMessageSaysTheCredentialChanged(t *testing.T) {
	err := asError(callFailure(vault.Account{Provider: "gmail", Alias: "work"}, &catalog.Provider{ID: "gmail", IdentityRoot: "google"}, proxy.ErrAccountChanged))

	if strings.Contains(err.Message, "removed") || !strings.Contains(err.Message, "credential") {
		t.Errorf("message = %q", err.Message)
	}
}
