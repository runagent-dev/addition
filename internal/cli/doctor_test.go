package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/vault"
)

const (
	fixInit      = "addition init"
	fixAddClient = "addition client add google --file client_secret.json"
	activateRoot = "https://console.developers.google.com/apis/api/"
)

type gotCheck struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Code    string `json:"code"`
	Summary string `json:"summary"`
	Fix     string `json:"fix"`
	Hint    string `json:"hint"`
}

type gotReport struct {
	OK      bool       `json:"ok"`
	Version string     `json:"version"`
	Home    string     `json:"home"`
	Checks  []gotCheck `json:"checks"`
	Next    *string    `json:"next"`
}

func decodeDoctor(t *testing.T, r result) gotReport {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.stdout), &raw); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\nstdout: %q", err, r.stdout)
	}
	for _, key := range []string{"ok", "version", "home", "checks", "next"} {
		if _, present := raw[key]; !present {
			t.Errorf("the report has no %q", key)
		}
	}
	if len(raw) != 5 {
		t.Errorf("the report has keys %v, want exactly ok, version, home, checks, next", raw)
	}
	return decodeJSON[gotReport](t, r)
}

func (rep gotReport) check(t *testing.T, id string) gotCheck {
	t.Helper()
	for _, c := range rep.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", id, rep.Checks)
	return gotCheck{}
}

func (rep gotReport) has(id string) bool {
	return slices.ContainsFunc(rep.Checks, func(c gotCheck) bool { return c.ID == id })
}

// wantNeedsAction checks the exit-3 contract: the report is on stdout and the envelope ends stderr.
func wantNeedsAction(t *testing.T, r result, next string) gotReport {
	t.Helper()
	if r.code != ExitNeedsConnect {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", r.code, ExitNeedsConnect, r.stdout, r.stderr)
	}
	if env := decodeError(t, lastLine(r.stderr)); env.Error.Code != "needs-action" {
		t.Errorf("error code = %q, want needs-action", env.Error.Code)
	}
	rep := decodeDoctor(t, r)
	if rep.OK {
		t.Error("ok = true, want false")
	}
	if rep.Next == nil || *rep.Next != next {
		t.Errorf("next = %v, want %q", rep.Next, next)
	}
	return rep
}

func wantHealthy(t *testing.T, r result) gotReport {
	t.Helper()
	if r.code != ExitOK || r.stderr != "" {
		t.Fatalf("exit = %d, stderr %q; want 0 and silence\nstdout: %s", r.code, r.stderr, r.stdout)
	}
	rep := decodeDoctor(t, r)
	if !rep.OK || rep.Next != nil || !strings.Contains(r.stdout, `"next":null`) {
		t.Errorf("ok = %v, next = %v; want ok with next null\n%s", rep.OK, rep.Next, r.stdout)
	}
	return rep
}

func TestDoctorBeforeInit(t *testing.T) {
	home := isolatedHome(t)

	rep := wantNeedsAction(t, run("doctor"), fixInit)

	if rep.Home != home || rep.Version == "" {
		t.Errorf("home %q, version %q; want %q and a version", rep.Home, rep.Version, home)
	}
	if c := rep.check(t, "vault"); c.Status != "fail" || c.Fix != fixInit || c.Summary == "" {
		t.Errorf("vault check = %+v", c)
	}
	if c := rep.check(t, "master-key"); c.Status != "skip" {
		t.Errorf("master-key check = %+v, want skip", c)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("doctor created %s", home)
	}
}

func TestDoctorWithAStaleHomeThatDoesNotExist(t *testing.T) {
	stale := filepath.Join(privateTempDir(t), "gone", "deeper")
	isolatedHome(t)
	t.Setenv("ADDITION_HOME", stale)

	rep := wantNeedsAction(t, run("doctor"), fixInit)

	if rep.Home != stale || rep.check(t, "vault").Status != "fail" {
		t.Errorf("home %q, vault %+v", rep.Home, rep.check(t, "vault"))
	}
}

func TestDoctorWithARelativeHome(t *testing.T) {
	isolatedHome(t)
	t.Setenv("ADDITION_HOME", "relative/addition")

	rep := wantNeedsAction(t, run("doctor"), "set ADDITION_HOME to an absolute path")

	if c := rep.check(t, "vault"); c.Status != "fail" || c.Fix != "set ADDITION_HOME to an absolute path" {
		t.Errorf("vault check = %+v", c)
	}
}

func TestDoctorTakesNoArguments(t *testing.T) {
	isolatedHome(t)
	r := run("doctor", "extra")
	if r.code != ExitUsage || r.stdout != "" {
		t.Errorf("exit %d, stdout %q; want usage error and no output", r.code, r.stdout)
	}
}

func TestDoctorAfterInitWithoutAClient(t *testing.T) {
	h := newHarness(t)

	rep := wantNeedsAction(t, h.run("doctor"), fixAddClient)

	if c := rep.check(t, "vault"); c.Status != "ok" {
		t.Errorf("vault check = %+v", c)
	}
	if c := rep.check(t, "master-key"); c.Status != "ok" || !strings.Contains(c.Summary, "file") {
		t.Errorf("master-key check = %+v, want ok naming the recorded source", c)
	}
	c := rep.check(t, "oauth-client:google")
	if c.Status != "warn" || c.Fix != fixAddClient || !strings.Contains(c.Hint, "Desktop app") {
		t.Errorf("oauth-client check = %+v, want a warning with the fix and a Desktop-app hint", c)
	}
	for _, id := range []string{"oauth-client:github", "oauth-client:slack"} {
		if rep.has(id) {
			t.Errorf("%s is reported although doctor has no setup hint for it", id)
		}
	}
}

func TestDoctorWithAClientAndNoAccounts(t *testing.T) {
	h := newHarness(t)
	h.addClient()

	rep := wantHealthy(t, h.run("doctor"))

	if c := rep.check(t, "oauth-client:google"); c.Status != "ok" || c.Fix != "" {
		t.Errorf("oauth-client check = %+v", c)
	}
	for _, c := range rep.Checks {
		if strings.HasPrefix(c.ID, "account:") || strings.HasPrefix(c.ID, "cloud-setup:") {
			t.Errorf("unexpected check %+v with no accounts", c)
		}
	}
}

func TestDoctorHealthyAccount(t *testing.T) {
	c := newCallHarness(t)

	rep := wantHealthy(t, c.run("doctor"))

	acct := rep.check(t, "account:gmail:work")
	if acct.Status != "ok" || acct.Code != "ok" || acct.Fix != "" {
		t.Errorf("account check = %+v", acct)
	}
	if rep.has("account:gmail:work:token-age") {
		t.Error("a fresh account has a token-age warning")
	}
	if n := len(c.api.requests()) + c.transport.total(); n != 0 {
		t.Errorf("doctor without --live made %d requests", n)
	}
}

func TestDoctorTreatsAnExpiredButRefreshableTokenAsOK(t *testing.T) {
	c := newCallHarness(t)
	c.expireToken()

	rep := wantHealthy(t, c.run("doctor"))

	if acct := rep.check(t, "account:gmail:work"); acct.Code != "ok" {
		t.Errorf("account check = %+v", acct)
	}
}

type failingTransport struct{ t *testing.T }

func (f failingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.t.Errorf("doctor sent a request to %s without --live", req.URL.Host)
	return nil, http.ErrNotSupported
}

func TestDoctorWithoutLiveMakesNoNetworkRequest(t *testing.T) {
	c := newCallHarness(t)
	c.attachApp("google-drive", "work", workSubject)
	c.expireToken()
	c.deps.httpClient = &http.Client{Transport: failingTransport{t}}

	wantHealthy(t, c.run("doctor"))
}

func TestDoctorWritesNoAuditEventWithoutLive(t *testing.T) {
	c := newCallHarness(t)
	before := auditEvents(t, c.home)

	c.run("doctor")

	if after := auditEvents(t, c.home); !slices.Equal(before, after) {
		t.Errorf("audit events %v became %v", before, after)
	}
}

func TestDoctorNeedsReauth(t *testing.T) {
	c := newCallHarness(t, func(g *fakeGoogle) { g.refreshError = "invalid_grant" })
	c.expireToken()
	c.run("call", "gmail", "-a", "work", "GET", "/x")

	rep := wantNeedsAction(t, c.run("doctor"), "addition connect gmail --as work")

	acct := rep.check(t, "account:gmail:work")
	if acct.Status != "fail" || acct.Code != "needs-reauth" || acct.Fix != "addition connect gmail --as work" {
		t.Errorf("account check = %+v", acct)
	}
}

func TestDoctorLegacyAccount(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	seedLegacyAccountWithExpiredCredential(t, h)

	rep := wantNeedsAction(t, h.run("doctor"), "addition accounts remove google:work")

	acct := rep.check(t, "account:google:work")
	if acct.Status != "fail" || acct.Code != "legacy" || !strings.Contains(acct.Summary, "addition connect gmail --as work") {
		t.Errorf("account check = %+v", acct)
	}
}

func TestDoctorMissingScopes(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.google.grant = func(requested []string) []string {
		return slices.DeleteFunc(slices.Clone(requested), func(s string) bool { return s == scopeGmailModify })
	}
	h.run("connect", "gmail", "--as", "work")

	rep := wantNeedsAction(t, h.run("doctor"), "addition connect gmail --as work")

	acct := rep.check(t, "account:gmail:work")
	if acct.Status != "fail" || acct.Code != "scopes-missing" || !strings.Contains(acct.Summary, scopeGmailModify) {
		t.Errorf("account check = %+v", acct)
	}
}

func seedAccount(t *testing.T, h *harness, cred vault.Credential) {
	t.Helper()
	ctx := context.Background()
	s, err := vault.Open(ctx, h.deps.env)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	acct, err := s.CreateAccount(ctx, vault.Account{Provider: "gmail", Alias: "work", Identity: workIdentity, Subject: workSubject,
		Scopes: []string{scopeOpenID, scopeEmail, scopeGmailRO, "https://www.googleapis.com/auth/gmail.send", scopeGmailModify}, Status: vault.AccountActive})
	if err != nil {
		t.Fatal(err)
	}
	if cred.AccessToken != "" {
		if err := s.PutCredential(ctx, acct.ID, cred); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDoctorAccountWithoutARefreshTokenOrCredential(t *testing.T) {
	cases := map[string]struct {
		cred vault.Credential
		code string
	}{
		"no refresh token": {vault.Credential{AccessToken: fakeAccessToken, TokenType: "Bearer"}, "no-refresh-token"},
		"no credential":    {vault.Credential{}, "no-credential"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.addClient()
			seedAccount(t, h, tc.cred)

			rep := wantNeedsAction(t, h.run("doctor"), "addition connect gmail --as work")

			if acct := rep.check(t, "account:gmail:work"); acct.Status != "fail" || acct.Code != tc.code {
				t.Errorf("account check = %+v, want code %s", acct, tc.code)
			}
		})
	}
}

func TestDoctorWarnsAboutTokenAgeWithTheInjectedClock(t *testing.T) {
	c := newCallHarness(t)

	c.clock.Advance(6*24*time.Hour - time.Minute)
	if rep := wantHealthy(t, c.run("doctor")); rep.has("account:gmail:work:token-age") {
		t.Error("a token-age warning appeared before 6 days")
	}

	c.clock.Advance(time.Minute)
	rep := wantHealthy(t, c.run("doctor"))

	age := rep.check(t, "account:gmail:work:token-age")
	if age.Status != "warn" || !strings.Contains(age.Hint, "Testing") || !strings.Contains(age.Hint, "7 days") {
		t.Errorf("token-age check = %+v", age)
	}
}

func TestDoctorCloudSetupHintsForEveryGoogleApp(t *testing.T) {
	c := newCallHarness(t)
	for _, app := range []string{"google-drive", "google-calendar", "google-chat", "google-docs"} {
		c.attachApp(app, "work", workSubject)
	}
	services := map[string]string{
		"gmail": "gmail.googleapis.com", "google-drive": "drive.googleapis.com", "google-calendar": "calendar-json.googleapis.com",
		"google-chat": "chat.googleapis.com", "google-docs": "docs.googleapis.com",
	}

	rep := wantHealthy(t, c.run("doctor"))

	for app, service := range services {
		hint := rep.check(t, "cloud-setup:"+app)
		if hint.Status != "warn" || !strings.Contains(hint.Hint, activateRoot+service+"/overview") || strings.Contains(hint.Hint, "project=") {
			t.Errorf("cloud-setup:%s = %+v, want the %s activation link without a project", app, hint, service)
		}
	}
}

func TestDoctorMasterKeyProblems(t *testing.T) {
	cases := map[string]struct {
		break_ func(t *testing.T, h *harness)
		fix    string
	}{
		"key file deleted": {func(t *testing.T, h *harness) {
			if err := os.Remove(filepath.Join(h.home, "master.key")); err != nil {
				t.Fatal(err)
			}
		}, "restore the master key (OS keychain entry, key file or ADDITION_MASTER_KEY)"},
		"wrong key in the environment": {func(t *testing.T, h *harness) {
			t.Setenv("ADDITION_MASTER_KEY", strings.Repeat("ab", 32))
		}, "set ADDITION_MASTER_KEY to the key this vault was created with"},
		"malformed key in the environment": {func(t *testing.T, h *harness) {
			t.Setenv("ADDITION_MASTER_KEY", "not-hex")
		}, "fix or unset ADDITION_MASTER_KEY"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			tc.break_(t, h)

			r := h.run("doctor")

			rep := wantNeedsAction(t, r, tc.fix)
			if c := rep.check(t, "master-key"); c.Status != "fail" || c.Fix != tc.fix {
				t.Errorf("master-key check = %+v", c)
			}
			if c := rep.check(t, "vault"); c.Status != "ok" {
				t.Errorf("vault check = %+v, want ok: the database is there", c)
			}
			assertNoLeak(t, r, "master.key", "not-hex", strings.Repeat("ab", 32))
		})
	}
}

func TestDoctorNeverPrintsTheMasterKey(t *testing.T) {
	h := newHarness(t)
	key, err := os.ReadFile(filepath.Join(h.home, "master.key"))
	if err != nil {
		t.Fatal(err)
	}

	r := h.run("doctor")

	assertNoLeak(t, r, strings.TrimSpace(string(key)))
}

func TestDoctorReportsTheFirstFixInPriorityOrder(t *testing.T) {
	h := newHarness(t)
	seedLegacyAccountWithExpiredCredential(t, h)

	rep := wantNeedsAction(t, h.run("doctor"), fixAddClient)

	if c := rep.check(t, "account:google:work"); c.Code != "legacy" {
		t.Errorf("the legacy account is not reported next to the missing client: %+v", c)
	}
}
