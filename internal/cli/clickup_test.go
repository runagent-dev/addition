package cli

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	clickUpWorkToken   = "pk_183_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	clickUpClientToken = "pk_907_ZYXWVUTSRQPONMLKJIHGFEDCBA543210"
)

// clickUpUsers maps each personal token to the body ClickUp's /api/v2/user answers it with.
var clickUpUsers = map[string]string{
	clickUpWorkToken:   `{"user":{"id":183,"username":"Ada","email":"ada@work.example"}}`,
	clickUpClientToken: `{"user":{"id":907,"username":"Ada","email":"ada@client.example"}}`,
}

// newClickUpHarness points the shipped clickup provider at a TLS fake API that accepts only the
// bare personal tokens above, as ClickUp does.
func newClickUpHarness(t *testing.T) (*tokenHarness, *fakeAPI) {
	t.Helper()
	th := newTokenHarness(t)
	api := newFakeAPI(t)
	th.secrets = append(th.secrets, clickUpWorkToken, clickUpClientToken)
	th.deps.catalogFS = editProvider(t, th.deps.catalogFS, "clickup", func(data []byte) []byte {
		data = mustReplace(t, data, `    - "api.clickup.com"`, `    - "`+api.host()+`"`)
		return mustReplace(t, data, `base_url: "https://api.clickup.com"`, `base_url: "https://`+api.host()+`"`)
	})
	th.deps.httpClient = &http.Client{Transport: &recordingTransport{base: api.srv.Client().Transport}, Timeout: 5 * time.Second}
	api.respond(func(n int, w http.ResponseWriter, r *http.Request) {
		user, ok := clickUpUsers[r.Header.Get("Authorization")]
		if !ok {
			status(http.StatusUnauthorized, `{"err":"Token invalid","ECODE":"OAUTH_019"}`, "Content-Type", "application/json")(n, w, r)
			return
		}
		switch r.URL.Path {
		case "/api/v2/user":
			jsonBody(user)(n, w, r)
		case "/api/v2/team":
			jsonBody(`{"teams":[{"id":"9001","name":"Work"}]}`)(n, w, r)
		default:
			status(http.StatusNotFound, `{}`, "Content-Type", "application/json")(n, w, r)
		}
	})
	return th, api
}

func (th *tokenHarness) connectClickUp(alias, token string) result {
	return th.runWithStdin(strings.NewReader(token+"\n"), "connect", "clickup", "--as", alias, "--token-stdin")
}

func TestConnectingClickUpWithAPersonalTokenNamesTheAccountByItsEmail(t *testing.T) {
	th, api := newClickUpHarness(t)

	r := th.connectClickUp("work", clickUpWorkToken)

	if r.code != ExitOK {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if got := decodeJSON[tokenConnectResult](t, r); got.Provider != "clickup" || got.Alias != "work" || got.Identity != "ada@work.example" || got.Method != "api_key" || !got.Created {
		t.Errorf("result = %+v", got)
	}
	reqs := api.requests()
	if len(reqs) != 1 || reqs[0].Method != "GET" || reqs[0].Path != "/api/v2/user" || reqs[0].Header.Get("Authorization") != clickUpWorkToken {
		t.Errorf("requests = %+v, want one GET /api/v2/user carrying the bare token", reqs)
	}
}

func TestSeveralClickUpAccountsStayApartAndEachCallUsesItsOwnToken(t *testing.T) {
	th, api := newClickUpHarness(t)
	for alias, token := range map[string]string{"work": clickUpWorkToken, "client": clickUpClientToken} {
		if r := th.connectClickUp(alias, token); r.code != ExitOK {
			t.Fatalf("connect %s: exit %d\nstderr: %s", alias, r.code, r.stderr)
		}
	}
	rows := decodeJSON[[]accountRow](t, th.mustRun("accounts", "list"))
	if len(rows) != 2 || rows[0].Provider != "clickup" || rows[1].Provider != "clickup" || rows[0].Alias == rows[1].Alias {
		t.Fatalf("accounts = %+v, want two clickup accounts", rows)
	}

	for alias, token := range map[string]string{"work": clickUpWorkToken, "client": clickUpClientToken} {
		api.mu.Lock()
		api.reqs = nil
		api.mu.Unlock()

		r := th.run("call", "clickup", "-a", alias, "GET", "/api/v2/team")

		if r.code != ExitOK || !strings.Contains(r.stdout, `"Work"`) {
			t.Fatalf("call -a %s: exit %d\nstdout: %s\nstderr: %s", alias, r.code, r.stdout, r.stderr)
		}
		if reqs := api.requests(); len(reqs) != 1 || reqs[0].Header.Get("Authorization") != token {
			t.Errorf("-a %s sent %+v, want only its own bare token", alias, reqs)
		}
	}
}

func TestAClickUpAliasCannotBeRepointedAtAnotherUsersToken(t *testing.T) {
	th, _ := newClickUpHarness(t)
	if r := th.connectClickUp("work", clickUpWorkToken); r.code != ExitOK {
		t.Fatalf("connect: exit %d", r.code)
	}
	before, _ := th.credentialOf("work")

	r := th.connectClickUp("work", clickUpClientToken)

	th.wantError(r, ExitUsage, "identity-mismatch")
	if after, _ := th.credentialOf("work"); after != before {
		t.Errorf("credential = %+v, want it untouched (%+v)", after, before)
	}
}

func TestClickUpRefusesATokenThatIsNotPersonalBeforeSendingAnything(t *testing.T) {
	th, api := newClickUpHarness(t)

	r := th.connectClickUp("work", "Bearer eyJhbGciOiJIUzI1NiJ9.payload.signature")

	env := th.wantError(r, ExitUsage, "token-prefix-mismatch")
	if !strings.Contains(env.Error.Message, `"pk_"`) || strings.Contains(r.stderr, "eyJhbGci") {
		t.Errorf("message = %q", env.Error.Message)
	}
	if n := len(api.requests()); n != 0 {
		t.Errorf("%d requests were sent for a token without the pk_ prefix", n)
	}
	th.wantNothingStored()
}

func TestAClickUpTokenThatIsRefusedStoresNothingAndPointsAtTheAppsPage(t *testing.T) {
	th, _ := newClickUpHarness(t)

	r := th.connectClickUp("work", "pk_0_NOTAREALTOKENNOTAREALTOKENNOTREAL")

	env := th.wantError(r, ExitNeedsConnect, "invalid-token")
	if !strings.Contains(env.Error.Message, "https://app.clickup.com/settings/apps") {
		t.Errorf("message = %q, want ClickUp's apps page", env.Error.Message)
	}
	th.wantNothingStored()
}

func TestClickUpCallsReachOnlyTheApiPaths(t *testing.T) {
	th, api := newClickUpHarness(t)
	if r := th.connectClickUp("work", clickUpWorkToken); r.code != ExitOK {
		t.Fatalf("connect: exit %d", r.code)
	}
	api.mu.Lock()
	api.reqs = nil
	api.mu.Unlock()

	th.wantError(th.run("call", "clickup", "-a", "work", "GET", "/somewhere/else"), ExitUsage, "path-not-allowed")

	if n := len(api.requests()); n != 0 {
		t.Errorf("%d requests left for a path outside /api/", n)
	}
}

func TestDoctorAsksForNoOAuthClientForClickUp(t *testing.T) {
	h := newHarness(t)

	rep := h.run("doctor")

	if strings.Contains(rep.stdout, "oauth-client:clickup") {
		t.Errorf("doctor reports an OAuth client for a token-only provider:\n%s", rep.stdout)
	}
}

func TestTheClickUpRecipeStartsFromTheWorkspaceListAndExplainsTheToken(t *testing.T) {
	r := run("guide", "clickup")

	if r.code != ExitOK {
		t.Fatalf("exit %d\nstderr: %s", r.code, r.stderr)
	}
	for _, want := range []string{
		"addition connect clickup --as", "--token-stdin", "pk_", "https://app.clickup.com/settings/apps",
		"/api/v2/team", "team_id", "429", "X-RateLimit-Reset",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("the ClickUp recipe never mentions %q", want)
		}
	}
}
