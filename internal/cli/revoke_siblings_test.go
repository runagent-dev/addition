package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/runagent-dev/addition/internal/vault"
)

type removedWithSiblings struct {
	Removed         bool     `json:"removed"`
	Revoked         bool     `json:"revoked"`
	AlsoInvalidated []string `json:"also_invalidated"`
}

// googleSiblingsHarness has gmail:work and google-drive:work for one Google user.
func googleSiblingsHarness(t *testing.T) (*callHarness, *fakeAPI) {
	t.Helper()
	c := newCallHarness(t, distinctTokensPerApp)
	return c, c.attachApp("google-drive", "work", workSubject)
}

func statusOf(t *testing.T, c *callHarness, ref string) string {
	t.Helper()
	return decodeJSON[accountRow](t, c.mustRun("accounts", "show", ref)).Status
}

func TestRemovingWithoutRevokeLeavesSiblingsActiveAndStaysOffline(t *testing.T) {
	c, drive := googleSiblingsHarness(t)

	out := decodeJSON[removedWithSiblings](t, c.mustRun("accounts", "remove", "gmail:work"))

	if !out.Removed || out.Revoked || len(out.AlsoInvalidated) != 0 {
		t.Errorf("remove = %+v, want removed locally with nothing revoked", out)
	}
	if c.transport.total() != 0 || len(c.google.revoked) != 0 {
		t.Errorf("%d requests left addition, %d revokes; a local removal makes no network call", c.transport.total(), len(c.google.revoked))
	}
	if got := statusOf(t, c, "google-drive:work"); got != "active" {
		t.Errorf("drive status = %q, want active", got)
	}
	c.mustRun("call", "drive", "-a", "work", "GET", "/drive/v3/files")
	if n := len(drive.requests()); n != 1 {
		t.Errorf("drive requests = %d, want 1", n)
	}
}

func TestRevokingAGrantSharedWithOtherAppsIsRefusedUntilAskedFor(t *testing.T) {
	c, _ := googleSiblingsHarness(t)

	env := c.wantError(c.run("accounts", "remove", "gmail:work", "--revoke"), ExitUsage, "revoke-affects-other-apps")

	for _, want := range []string{"google-drive:work", "--and-siblings"} {
		if !strings.Contains(env.Error.Message, want) {
			t.Errorf("message %q does not mention %q", env.Error.Message, want)
		}
	}
	if len(c.google.revoked) != 0 || c.transport.total() != 0 {
		t.Errorf("%d revokes and %d requests; a refused removal must not touch the network", len(c.google.revoked), c.transport.total())
	}
	c.wantAccounts(2)
	if got := statusOf(t, c, "google-drive:work"); got != "active" {
		t.Errorf("drive status = %q, want active", got)
	}
}

func TestRevokingWithSiblingsSendsOneRevokeAndInvalidatesThemLocally(t *testing.T) {
	c, drive := googleSiblingsHarness(t)

	out := decodeJSON[removedWithSiblings](t, c.mustRun("accounts", "remove", "gmail:work", "--revoke", "--and-siblings"))

	if !out.Removed || !out.Revoked || !slices.Equal(out.AlsoInvalidated, []string{"google-drive:work"}) {
		t.Errorf("remove = %+v, want revoked with google-drive:work invalidated", out)
	}
	if !slices.Equal(c.google.revoked, []string{fakeRefreshToken + "-gmail"}) {
		t.Errorf("revoked %v, want exactly gmail's refresh token once", c.google.revoked)
	}
	rows := c.wantAccounts(1)
	if rows[0].Provider != "google-drive" || rows[0].Status != "needs-reauth" {
		t.Errorf("remaining accounts = %+v, want drive marked needs-reauth", rows)
	}
	c.transport.reset()
	c.wantError(c.run("call", "drive", "-a", "work", "GET", "/drive/v3/files"), ExitNeedsConnect, "needs-reauth")
	if len(drive.requests()) != 0 || c.transport.total() != 0 {
		t.Errorf("an invalidated sibling sent %d requests to the API and %d in all", len(drive.requests()), c.transport.total())
	}
	if events := auditEvents(t, c.home); !slices.Equal(events[len(events)-2:], []string{"revoke", "revoke"}) {
		t.Errorf("audit events = %v, want a revoke row for each invalidated account", events)
	}
}

func TestRevokingWithoutSiblingsWorksAsBefore(t *testing.T) {
	c := newCallHarness(t)

	out := decodeJSON[removedWithSiblings](t, c.mustRun("accounts", "remove", "gmail:work", "--revoke"))

	if !out.Removed || !out.Revoked || out.AlsoInvalidated == nil || len(out.AlsoInvalidated) != 0 {
		t.Errorf("remove = %+v, want revoked with an empty also_invalidated list", out)
	}
	if !slices.Equal(c.google.revoked, []string{fakeRefreshToken}) {
		t.Errorf("revoked %v, want the refresh token", c.google.revoked)
	}
	c.wantAccounts(0)
}

func TestASiblingOfAnotherPersonIsNotAffected(t *testing.T) {
	c := newCallHarness(t, distinctTokensPerApp)
	c.attachApp("google-drive", "work", "2002")

	out := decodeJSON[removedWithSiblings](t, c.mustRun("accounts", "remove", "gmail:work", "--revoke"))

	if !out.Revoked || len(out.AlsoInvalidated) != 0 {
		t.Errorf("remove = %+v, want a plain revoke", out)
	}
	if got := statusOf(t, c, "google-drive:work"); got != "active" {
		t.Errorf("drive status = %q, want active: it belongs to another Google user", got)
	}
}

func TestAnAliasOfTheSameUserInTheSameAppIsAlsoASibling(t *testing.T) {
	c := newCallHarness(t, distinctTokensPerApp)
	c.connect("home", "gmail")

	env := c.wantError(c.run("accounts", "remove", "gmail:work", "--revoke"), ExitUsage, "revoke-affects-other-apps")

	if !strings.Contains(env.Error.Message, "gmail:home") {
		t.Errorf("message %q does not name gmail:home", env.Error.Message)
	}
}

func seedLegacyGoogleAccount(t *testing.T, c *callHarness, alias, subject string) {
	t.Helper()
	s, err := vault.Open(context.Background(), c.deps.env)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateAccount(context.Background(), vault.Account{Provider: "google", Alias: alias, Identity: workIdentity, Subject: subject, Scopes: gmailScopes}); err != nil {
		t.Fatal(err)
	}
}

func TestALegacyGoogleRowIsASiblingOfEveryApp(t *testing.T) {
	c := newCallHarness(t, distinctTokensPerApp)
	seedLegacyGoogleAccount(t, c, "old", workSubject)

	toApp := c.wantError(c.run("accounts", "remove", "gmail:work", "--revoke"), ExitUsage, "revoke-affects-other-apps")
	toLegacy := c.wantError(c.run("accounts", "remove", "google:old", "--revoke"), ExitUsage, "revoke-affects-other-apps")

	if !strings.Contains(toApp.Error.Message, "google:old") || !strings.Contains(toLegacy.Error.Message, "gmail:work") {
		t.Errorf("messages %q and %q must name each other's accounts", toApp.Error.Message, toLegacy.Error.Message)
	}
	out := decodeJSON[removedWithSiblings](t, c.mustRun("accounts", "remove", "gmail:work", "--revoke", "--and-siblings"))
	if !slices.Equal(out.AlsoInvalidated, []string{"google:old"}) {
		t.Errorf("also_invalidated = %v, want the legacy row", out.AlsoInvalidated)
	}
}

func TestAndSiblingsWithoutRevokeIsAUsageError(t *testing.T) {
	c, _ := googleSiblingsHarness(t)

	c.wantError(c.run("accounts", "remove", "gmail:work", "--and-siblings"), ExitUsage, "usage")

	c.wantAccounts(2)
}

func TestAFailedRevokeInvalidatesNoSibling(t *testing.T) {
	c, _ := googleSiblingsHarness(t)
	c.google.configure(func(g *fakeGoogle) { g.revokeFails = true })

	out := decodeJSON[removedWithSiblings](t, c.mustRun("accounts", "remove", "gmail:work", "--revoke", "--and-siblings"))

	if !out.Removed || out.Revoked || len(out.AlsoInvalidated) != 0 {
		t.Errorf("remove = %+v, want removed locally, not revoked, nothing invalidated", out)
	}
	if got := statusOf(t, c, "google-drive:work"); got != "active" {
		t.Errorf("drive status = %q, want active: the provider rejected the revoke", got)
	}
}
