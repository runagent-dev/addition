package cli

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

type tokenConnectResult struct {
	Provider string   `json:"provider"`
	Alias    string   `json:"alias"`
	Identity string   `json:"identity"`
	Scopes   []string `json:"scopes"`
	Created  bool     `json:"created"`
	Method   string   `json:"method"`
}

type tokenAccountRow struct {
	Provider        string   `json:"provider"`
	Alias           string   `json:"alias"`
	Identity        string   `json:"identity"`
	Method          string   `json:"method"`
	Scopes          []string `json:"scopes"`
	ScopesReady     bool     `json:"scopes_ready"`
	MissingScopes   []string `json:"missing_scopes"`
	Status          string   `json:"status"`
	ExpiresAt       *string  `json:"expires_at"`
	HasRefreshToken bool     `json:"has_refresh_token"`
}

func TestConnectWithATokenOnStdinStoresTheAccountAndNeedsNoOAuthClient(t *testing.T) {
	th := newTokenHarness(t)

	r := th.connectToken("work", pastedToken)

	if r.code != ExitOK {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	got := decodeJSON[tokenConnectResult](t, r)
	want := tokenConnectResult{Provider: tokenApp, Alias: "work", Identity: tokenIdentity, Scopes: []string{}, Created: true, Method: "api_key"}
	if got.Provider != want.Provider || got.Alias != want.Alias || got.Identity != want.Identity || !got.Created || got.Method != "api_key" || got.Scopes == nil || len(got.Scopes) != 0 {
		t.Errorf("result = %+v, want %+v", got, want)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want silence", r.stderr)
	}
	reqs := th.api.requests()
	if len(reqs) != 1 || reqs[0].Method != "GET" || reqs[0].Path != "/v1/me" || reqs[0].Header.Get("Authorization") != "Bearer "+pastedToken {
		t.Fatalf("the token was verified with %+v, want one GET /v1/me with a Bearer header", reqs)
	}
	if cred, ok := th.credentialOf("work"); !ok || cred.Kind != "api_key" || cred.HasRefresh || cred.HasExpiry || cred.Revision != 1 {
		t.Errorf("credential = %+v, %v", cred, ok)
	}
	if rows := th.auditRows(); len(rows) != 2 || rows[1] != "connect {}" {
		t.Errorf("audit rows = %q, want init then a bare connect event", rows)
	}
}

func TestConnectTrimsTheLineAndReadsOnlyTheFirst(t *testing.T) {
	th := newTokenHarness(t)

	r := th.runWithStdin(strings.NewReader("  "+pastedToken+" \r\nsecond line\n"), "connect", tokenApp, "--as", "work", "--token-stdin")

	if r.code != ExitOK {
		t.Fatalf("exit %d\nstderr: %s", r.code, r.stderr)
	}
	if got := th.api.requests()[0].Header.Get("Authorization"); got != "Bearer "+pastedToken {
		t.Errorf("Authorization = %q, want the trimmed first line", got)
	}
}

func TestConnectWithATokenFromTheEnvironment(t *testing.T) {
	th := newTokenHarness(t)
	t.Setenv("TOKENAPP_TOKEN", pastedToken)

	r := th.runWithStdin(neverReadStdin{t}, "connect", tokenApp, "--as", "work", "--token-env", "TOKENAPP_TOKEN")

	if r.code != ExitOK {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if got := decodeJSON[tokenConnectResult](t, r); got.Method != "api_key" || got.Alias != "work" {
		t.Errorf("result = %+v", got)
	}
	if cred, ok := th.credentialOf("work"); !ok || cred.Kind != "api_key" {
		t.Errorf("credential = %+v, %v", cred, ok)
	}
}

func TestTokenEnvProblemsAreUsageErrors(t *testing.T) {
	th := newTokenHarness(t)
	t.Setenv("TOKENAPP_EMPTY", "")
	t.Setenv("TOKENAPP_SPACES", "   ")
	cases := map[string]struct{ name, code string }{
		"unset":         {"TOKENAPP_UNSET_FOR_TEST", "token-env-unset"},
		"set but empty": {"TOKENAPP_EMPTY", "empty-token"},
		"only spaces":   {"TOKENAPP_SPACES", "empty-token"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := th.run("connect", tokenApp, "--as", "work", "--token-env", tc.name)

			env := th.wantError(r, ExitUsage, tc.code)
			if env.Error.Message == "" {
				t.Error("no message")
			}
		})
	}
	if n := len(th.api.requests()); n != 0 {
		t.Errorf("%d requests were sent for a token that was never read", n)
	}
}

func TestAnEmptyOrMalformedStdinTokenIsRefusedWithoutEchoingIt(t *testing.T) {
	th := newTokenHarness(t)
	cases := map[string]struct{ stdin, code string }{
		"empty stdin":       {"", "empty-token"},
		"only a newline":    {"\n", "empty-token"},
		"space inside":      {"pk_has a space\n", "bad-token"},
		"control character": {"pk_bad\x01token\n", "bad-token"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := th.runWithStdin(strings.NewReader(tc.stdin), "connect", tokenApp, "--as", "work", "--token-stdin")

			th.wantError(r, ExitUsage, tc.code)
			if strings.Contains(r.stderr, "has a space") || strings.Contains(r.stderr, "bad\x01token") {
				t.Errorf("the error echoes the token: %q", r.stderr)
			}
		})
	}
	if n := len(th.api.requests()); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
}

func TestAPrefixMismatchIsRefusedBeforeAnythingIsSentAndWithoutEchoingTheToken(t *testing.T) {
	th := newTokenHarness(t)

	r := th.runWithStdin(strings.NewReader("sk_live_not-the-right-kind\n"), "connect", tokenApp, "--as", "work", "--token-stdin")

	env := th.wantError(r, ExitUsage, "token-prefix-mismatch")
	if !strings.Contains(env.Error.Message, `"pk_"`) || strings.Contains(r.stderr, "sk_live") {
		t.Errorf("message = %q, want it to name the expected prefix and never the token", env.Error.Message)
	}
	if n := len(th.api.requests()); n != 0 {
		t.Errorf("%d requests were sent for a token with the wrong prefix", n)
	}
	th.wantNothingStored()
}

func TestARejectedTokenStoresNothingAndPointsAtWhereToCreateOne(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			th := newTokenHarness(t)
			th.api.respond(func(_ int, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"` + r.Header.Get("Authorization") + `"}`))
			})

			r := th.connectToken("work", pastedToken)

			env := th.wantError(r, ExitNeedsConnect, "invalid-token")
			if !strings.Contains(env.Error.Message, createURL) || !strings.Contains(env.Error.Message, "--token-stdin") {
				t.Errorf("message = %q, want the create URL and the command to retry", env.Error.Message)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want nothing", r.stdout)
			}
			th.wantNothingStored()
			if rows := th.auditRows(); len(rows) != 1 {
				t.Errorf("audit rows = %q, want only init: nothing connected", rows)
			}
		})
	}
}

func TestOtherVerificationFailuresAreUpstreamErrorsAndStoreNothing(t *testing.T) {
	cases := map[string]struct {
		handle func(int, http.ResponseWriter, *http.Request)
		exit   int
		code   string
	}{
		"not found":     {status(http.StatusNotFound, `{}`, "Content-Type", "application/json"), ExitUpstreamClient, "identity-rejected"},
		"server error":  {status(http.StatusBadGateway, `{}`, "Content-Type", "application/json"), ExitUpstreamServer, "identity-rejected"},
		"no subject":    {jsonBody(`{"login":"octo"}`), ExitUpstreamClient, "identity-rejected"},
		"no display":    {jsonBody(`{"id":1}`), ExitUpstreamClient, "identity-rejected"},
		"redirect away": {nil, ExitUpstreamClient, "identity-rejected"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			th := newTokenHarness(t)
			handle := tc.handle
			if handle == nil {
				handle = status(http.StatusFound, "", "Location", th.evil.url("/steal"))
			}
			th.api.respond(handle)

			r := th.connectToken("work", pastedToken)

			th.wantError(r, tc.exit, tc.code)
			th.wantNothingStored()
			if n := len(th.evil.requests()); n != 0 {
				t.Errorf("a redirect was followed: %d requests reached another host", n)
			}
		})
	}
}

func TestAnUnreachableProviderIsANetworkErrorAndStoresNothing(t *testing.T) {
	th := newTokenHarness(t)
	th.api.srv.Close()

	r := th.connectToken("work", pastedToken)

	th.wantError(r, ExitUpstreamServer, "network")
	th.wantNothingStored()
}

func TestAnIdentityResponseThatRepeatsTheTokenIsWithheld(t *testing.T) {
	cases := map[string]string{
		"as the display name": `{"id":7,"login":"` + pastedToken + `"}`,
		"as the subject":      `{"id":"` + pastedToken + `","login":"octo"}`,
		"inside a long name":  `{"id":7,"login":"octo-` + pastedToken + `-x"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			th := newTokenHarness(t)
			th.api.respond(jsonBody(body))

			r := th.connectToken("work", pastedToken)

			th.wantError(r, ExitUpstreamServer, "credential-echo")
			th.wantNothingStored()
		})
	}
}

func TestReconnectingWithANewTokenReplacesTheCredentialAndItsRevision(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)
	before, _ := th.credentialOf("work")
	th.accept(secondToken)

	r := th.connectToken("work", secondToken)

	if r.code != ExitOK {
		t.Fatalf("exit %d\nstderr: %s", r.code, r.stderr)
	}
	if got := decodeJSON[tokenConnectResult](t, r); got.Created || got.Method != "api_key" {
		t.Errorf("result = %+v, want created false", got)
	}
	after, _ := th.credentialOf("work")
	if after.Revision != before.Revision+1 || after.Kind != "api_key" {
		t.Errorf("credential = %+v after %+v, want the next revision", after, before)
	}
	th.transport.reset()
	if r := th.call("GET", "/v1/me"); r.code != ExitOK {
		t.Fatalf("call with the replaced token: exit %d\nstderr: %s", r.code, r.stderr)
	}
	reqs := th.api.requests()
	if got := reqs[len(reqs)-1].Header.Get("Authorization"); got != "Bearer "+secondToken {
		t.Errorf("Authorization = %q, want the new token", got)
	}
	if rows := decodeJSON[[]tokenAccountRow](t, th.mustRun("accounts", "list")); len(rows) != 1 {
		t.Errorf("accounts = %+v, want the one alias", rows)
	}
}

func TestReconnectingARejectedAccountMakesItActiveAgain(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)
	th.accept(secondToken)
	th.wantError(th.call("GET", "/v1/me"), ExitNeedsConnect, "needs-reauth")

	th.mustConnectToken("work", secondToken)

	rows := decodeJSON[[]tokenAccountRow](t, th.mustRun("accounts", "list"))
	if len(rows) != 1 || rows[0].Status != "active" {
		t.Errorf("accounts = %+v, want one active account", rows)
	}
	if r := th.call("GET", "/v1/me"); r.code != ExitOK {
		t.Errorf("call after reconnecting: exit %d\nstderr: %s", r.code, r.stderr)
	}
}

func TestAnAliasStaysBoundToItsSubject(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)
	before, _ := th.credentialOf("work")
	th.accept(secondToken)
	th.setSubject("9999")

	r := th.connectToken("work", secondToken)

	th.wantError(r, ExitUsage, "identity-mismatch")
	if after, _ := th.credentialOf("work"); after != before {
		t.Errorf("credential = %+v, want it untouched (%+v)", after, before)
	}
}

func TestATokenAccountWithoutAnIdentityEndpointIsVerifiedAtTheProbePath(t *testing.T) {
	th := newTokenHarness(t, tokenProviderOpts{noIdentity: true})

	r := th.connectToken("work", pastedToken)

	if r.code != ExitOK {
		t.Fatalf("exit %d\nstderr: %s", r.code, r.stderr)
	}
	if got := decodeJSON[tokenConnectResult](t, r); got.Identity != "" || got.Method != "api_key" {
		t.Errorf("result = %+v, want an empty identity", got)
	}
	if reqs := th.api.requests(); len(reqs) != 1 || reqs[0].Path != "/v1/me" {
		t.Errorf("requests = %+v, want one GET of the probe path", reqs)
	}
	if rows := decodeJSON[[]tokenAccountRow](t, th.mustRun("accounts", "list")); len(rows) != 1 || rows[0].Identity != "" {
		t.Errorf("accounts = %+v", rows)
	}
}

func TestABareTokenProviderSendsNoSchemeWord(t *testing.T) {
	th := newTokenHarness(t, tokenProviderOpts{bare: true})

	th.mustConnectToken("work", pastedToken)
	r := th.call("GET", "/v1/me")

	if r.code != ExitOK {
		t.Fatalf("call: exit %d\nstderr: %s", r.code, r.stderr)
	}
	reqs := th.api.requests()
	if len(reqs) == 0 || reqs[len(reqs)-1].Header.Get("Authorization") != pastedToken {
		t.Errorf("Authorization = %+v, want the bare token", reqs)
	}
}

func TestMethodSelection(t *testing.T) {
	th := newTokenHarness(t)

	t.Run("a token flag forces api_key on an OAuth-only provider that lacks it", func(t *testing.T) {
		r := th.runWithStdin(strings.NewReader(pastedToken+"\n"), "connect", "gmail", "--as", "work", "--token-stdin")
		th.wantError(r, ExitUsage, "method-not-supported")
	})
	t.Run("a token provider has no browser flow", func(t *testing.T) {
		th.wantError(th.run("connect", tokenApp, "--as", "work", "--no-browser"), ExitUsage, "method-not-supported")
		th.wantError(th.run("connect", tokenApp, "--as", "work", "--method", "oauth2"), ExitUsage, "method-not-supported")
	})
	t.Run("an unknown method", func(t *testing.T) {
		th.wantError(th.run("connect", tokenApp, "--as", "work", "--method", "magic"), ExitUsage, "usage")
	})
	t.Run("token flags and the OAuth method conflict", func(t *testing.T) {
		r := th.runWithStdin(strings.NewReader(pastedToken+"\n"), "connect", tokenApp, "--as", "work", "--token-stdin", "--method", "oauth2")
		th.wantError(r, ExitUsage, "usage")
	})
	t.Run("the two token sources conflict", func(t *testing.T) {
		r := th.runWithStdin(strings.NewReader(pastedToken+"\n"), "connect", tokenApp, "--as", "work", "--token-stdin", "--token-env", "X")
		th.wantError(r, ExitUsage, "usage")
	})
	t.Run("token flags do not combine with --complete", func(t *testing.T) {
		th.wantError(th.run("connect", "--complete", "http://127.0.0.1:1/callback?code=a&state=b", "--token-stdin"), ExitUsage, "usage")
	})
	if n := len(th.api.requests()); n != 0 {
		t.Errorf("%d requests were sent while refusing", n)
	}
	th.wantNothingStored()
}

func TestATokenIsNeverAcceptedAsAFlagValue(t *testing.T) {
	th := newTokenHarness(t)
	const typed = "pk_typed_on_the_command_line"

	for _, flag := range []string{"--token", "--api-key", "--token-value"} {
		r := run("connect", tokenApp, "--as", "work", flag, typed)

		if r.code != ExitUsage {
			t.Errorf("%s: exit %d, want a usage error", flag, r.code)
		}
		assertNoLeak(t, r, typed)
	}
	th.wantNothingStored()
}

func TestWithoutATTYOrAFlagConnectNeverPrompts(t *testing.T) {
	th := newTokenHarness(t)

	r := th.runWithStdin(neverReadStdin{t}, "connect", tokenApp, "--as", "work")

	env := th.wantError(r, ExitUsage, "token-required")
	for _, want := range []string{"--token-stdin", "--token-env", createURL} {
		if !strings.Contains(env.Error.Message, want) {
			t.Errorf("message = %q, want it to mention %s", env.Error.Message, want)
		}
	}
	th.wantNothingStored()
}

type fakePrompter struct {
	interactive bool
	secret      string
	err         error
	reads       int
}

func (p *fakePrompter) Interactive() bool { return p.interactive }

func (p *fakePrompter) ReadSecret() (string, error) {
	p.reads++
	return p.secret, p.err
}

func TestOnATerminalConnectPromptsWithoutEchoAndKeepsStdoutJSON(t *testing.T) {
	th := newTokenHarness(t)
	prompter := &fakePrompter{interactive: true, secret: pastedToken}
	th.deps.prompter = prompter

	r := th.run("connect", tokenApp, "--as", "work")

	if r.code != ExitOK {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if prompter.reads != 1 {
		t.Errorf("the prompt was read %d times, want once", prompter.reads)
	}
	if got := decodeJSON[tokenConnectResult](t, r); got.Method != "api_key" || got.Identity != tokenIdentity {
		t.Errorf("result = %+v", got)
	}
	for _, want := range []string{createURL, "Open the page."} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr = %q, want the instructions to mention %q", r.stderr, want)
		}
	}
}

func TestAPromptFailureIsNotATokenAndNothingIsSent(t *testing.T) {
	th := newTokenHarness(t)
	th.deps.prompter = &fakePrompter{interactive: true, err: errors.New("terminal went away")}

	r := th.run("connect", tokenApp, "--as", "work")

	th.wantError(r, ExitUsage, "token-required")
	if n := len(th.api.requests()); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
}

func TestTokenFlagsNeverPromptEvenOnATerminal(t *testing.T) {
	th := newTokenHarness(t)
	prompter := &fakePrompter{interactive: true, secret: secondToken}
	th.deps.prompter = prompter

	r := th.connectToken("work", pastedToken)

	if r.code != ExitOK || prompter.reads != 0 {
		t.Errorf("exit %d, prompts %d; want the flag's token and no prompt", r.code, prompter.reads)
	}
}

func TestConnectingATokenProviderWithBothMethodsDefaultsToTheToken(t *testing.T) {
	th := newTokenHarness(t)

	r := th.run("connect", "github", "--as", "me")

	env := th.wantError(r, ExitUsage, "token-required")
	if !strings.Contains(env.Error.Message, "https://github.com/settings/tokens") {
		t.Errorf("message = %q, want github's create URL", env.Error.Message)
	}
}

func TestGitHubTokenConnectUsesItsIdentityEndpoint(t *testing.T) {
	th := newTokenHarness(t)
	api := newFakeAPI(t)
	th.secrets = append(th.secrets, "ghp_exampleExampleExample0123")
	th.deps.catalogFS = editProvider(t, th.deps.catalogFS, "github", func(data []byte) []byte {
		data = mustReplace(t, data, "    - \"api.github.com\"", "    - \"api.github.com\"\n    - \""+api.host()+"\"")
		return mustReplace(t, data, `base_url: "https://api.github.com"`, `base_url: "https://`+api.host()+`"`)
	})
	th.deps.httpClient = &http.Client{Transport: &recordingTransport{base: api.srv.Client().Transport}, Timeout: 5 * time.Second}
	api.respond(requireTokenBody("ghp_exampleExampleExample0123", `{"id":583231,"login":"octocat"}`))

	r := th.runWithStdin(strings.NewReader("ghp_exampleExampleExample0123\n"), "connect", "github", "--as", "me", "--token-stdin")

	if r.code != ExitOK {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if got := decodeJSON[tokenConnectResult](t, r); got.Identity != "octocat" || got.Method != "api_key" || got.Provider != "github" {
		t.Errorf("result = %+v", got)
	}
	if reqs := api.requests(); len(reqs) != 1 || reqs[0].Path != "/user" {
		t.Errorf("requests = %+v, want GET /user", reqs)
	}
}

// requireTokenBody answers body to the bearer token and 401 to anything else.
func requireTokenBody(token, body string) func(int, http.ResponseWriter, *http.Request) {
	return func(n int, w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			status(http.StatusUnauthorized, `{}`, "Content-Type", "application/json")(n, w, r)
			return
		}
		jsonBody(body)(n, w, r)
	}
}

func TestEveryAuditRowOfATokenSessionIsFreeOfSecretsPathsAndQueries(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)
	th.mustRun("call", tokenApp, "GET", "/v1/me", "--query", "secret_query=1")

	rows := th.auditRows()

	want := []string{"init", "connect {}", `call {"method":"GET","status":"200"}`}
	if len(rows) != 3 || !strings.HasPrefix(rows[0], "init ") || !slices.Equal(rows[1:], want[1:]) {
		t.Errorf("audit rows = %q, want %q", rows, want)
	}
	for _, row := range rows {
		if strings.Contains(row, pastedToken) || strings.Contains(row, "secret_query") || strings.Contains(row, "/v1/me") {
			t.Errorf("audit row %q holds a secret, a path or a query", row)
		}
	}
}

func TestTheTokenIsInTheVaultOnlyAsASealedBlob(t *testing.T) {
	th := newTokenHarness(t)
	th.mustConnectToken("work", pastedToken)

	if dump := dumpDB(t, th.home); strings.Contains(dump, pastedToken) {
		t.Error("the token is in the vault in plaintext")
	}
}
