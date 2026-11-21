package cli

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type accountRow struct {
	Provider        string   `json:"provider"`
	Alias           string   `json:"alias"`
	Identity        string   `json:"identity"`
	Scopes          []string `json:"scopes"`
	Status          string   `json:"status"`
	ExpiresAt       *string  `json:"expires_at"`
	HasRefreshToken bool     `json:"has_refresh_token"`
	Default         bool     `json:"default"`
}

type removed struct {
	Provider string `json:"provider"`
	Alias    string `json:"alias"`
	Removed  bool   `json:"removed"`
	Revoke   struct {
		Attempted bool   `json:"attempted"`
		OK        bool   `json:"ok"`
		Error     string `json:"error"`
	} `json:"revoke"`
}

func (h *harness) wantAccounts(n int) []accountRow {
	h.t.Helper()
	rows := decodeJSON[[]accountRow](h.t, h.mustRun("accounts", "list"))
	if len(rows) != n {
		h.t.Fatalf("accounts list has %d rows, want %d: %+v", len(rows), n, rows)
	}
	return rows
}

func (h *harness) connect(alias, app string) {
	h.t.Helper()
	h.mustRun("connect", app, "--as", alias)
}

func TestAccountsListAndShow(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	if r := h.mustRun("accounts", "list"); strings.TrimSpace(r.stdout) != "[]" {
		t.Errorf("empty list = %q, want []", r.stdout)
	}
	h.connect("work", "gmail")

	rows := h.wantAccounts(1)
	row := rows[0]
	if row.Provider != "gmail" || row.Alias != "work" || row.Identity != workIdentity || row.Status != "active" ||
		!row.HasRefreshToken || row.Default || !containsAll(row.Scopes, gmailScopes...) {
		t.Errorf("row = %+v", row)
	}
	wantExpiry := h.clock.Now().Add(3599 * time.Second)
	if row.ExpiresAt == nil {
		t.Fatal("expires_at is null")
	} else if exp, err := time.Parse(time.RFC3339, *row.ExpiresAt); err != nil || !exp.Equal(wantExpiry) {
		t.Errorf("expires_at = %s, want %s", *row.ExpiresAt, wantExpiry)
	}

	for _, ref := range []string{"gmail:work"} {
		show := decodeJSON[accountRow](t, h.mustRun("accounts", "show", ref))
		if show.Alias != "work" || show.Identity != workIdentity {
			t.Errorf("show %s = %+v", ref, show)
		}
	}
	h.wantError(h.run("accounts", "show", "gmail:ghost"), ExitNeedsConnect, "not-connected")
	h.wantError(h.run("accounts", "show", "work"), ExitUsage, "usage")
	h.wantError(h.run("accounts", "show", "nope:work"), ExitUsage, "unknown-provider")
}

func TestAccountsDefault(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.connect("work", "gmail")
	h.connect("personal", "gmail")

	r := h.mustRun("accounts", "default", "gmail:personal")
	out := decodeJSON[map[string]string](t, r)
	if out["provider"] != "gmail" || out["default"] != "personal" {
		t.Errorf("default = %v", out)
	}
	for _, row := range h.wantAccounts(2) {
		if row.Default != (row.Alias == "personal") {
			t.Errorf("%s default = %v", row.Alias, row.Default)
		}
	}
	h.wantError(h.run("accounts", "default", "gmail:ghost"), ExitNeedsConnect, "not-connected")
}

func TestAccountsRemoveWithRevoke(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.connect("work", "gmail")

	r := h.mustRun("accounts", "remove", "gmail:work", "--revoke")

	out := decodeJSON[removed](t, r)
	if !out.Removed || !out.Revoke.Attempted || !out.Revoke.OK {
		t.Errorf("remove = %+v", out)
	}
	if !slices.Equal(h.google.revoked, []string{fakeRefreshToken}) {
		t.Errorf("revoked %d tokens, want the refresh token", len(h.google.revoked))
	}
	h.wantAccounts(0)
	if rows := auditEvents(t, h.home); !slices.Equal(rows, []string{"init", "connect", "revoke"}) {
		t.Errorf("audit events = %v", rows)
	}
	h.wantError(h.run("accounts", "remove", "gmail:work"), ExitNeedsConnect, "not-connected")
	assertAuditHoldsNoSecrets(t, h)
}

func TestAccountsRemoveSurvivesAFailedRevoke(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.connect("work", "gmail")
	h.google.revokeFails = true

	out := decodeJSON[removed](t, h.mustRun("accounts", "remove", "gmail:work", "--revoke"))

	if !out.Removed || !out.Revoke.Attempted || out.Revoke.OK || out.Revoke.Error == "" {
		t.Errorf("remove = %+v", out)
	}
	h.wantAccounts(0)
}

func TestAccountsRemoveLocalOnly(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.connect("work", "gmail")
	h.mustRun("accounts", "default", "gmail:work")

	out := decodeJSON[removed](t, h.mustRun("accounts", "remove", "gmail:work"))

	if !out.Removed || out.Revoke.Attempted {
		t.Errorf("remove = %+v", out)
	}
	if len(h.google.revoked) != 0 {
		t.Error("remove without --revoke called the revoke endpoint")
	}
	if rows := auditEvents(t, h.home); !slices.Equal(rows, []string{"init", "connect", "revoke"}) {
		t.Errorf("audit events = %v", rows)
	}
	h.connect("work", "gmail")
	if h.wantAccounts(1)[0].Default {
		t.Error("a removed default came back with its alias")
	}
}

type authCheck struct {
	Provider        string   `json:"provider"`
	Alias           string   `json:"alias"`
	Connected       bool     `json:"connected"`
	Status          string   `json:"status"`
	AccessToken     string   `json:"access_token"`
	ExpiresAt       *string  `json:"expires_at"`
	HasRefreshToken bool     `json:"has_refresh_token"`
	Scopes          []string `json:"scopes"`
	ScopesReady     bool     `json:"scopes_ready"`
	MissingScopes   []string `json:"missing_scopes"`
}

func TestAuthCheck(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.connect("work", "gmail")

	checks := decodeJSON[[]authCheck](t, h.mustRun("accounts", "check"))
	if len(checks) != 1 {
		t.Fatalf("checks = %+v", checks)
	}
	c := checks[0]
	if c.Provider != "gmail" || c.Alias != "work" || !c.Connected || c.Status != "active" || c.AccessToken != "valid" || !c.HasRefreshToken {
		t.Errorf("check = %+v", c)
	}
	if !c.ScopesReady || c.MissingScopes == nil || len(c.MissingScopes) != 0 {
		t.Errorf("scopes_ready %v, missing_scopes %v; want ready and an empty list", c.ScopesReady, c.MissingScopes)
	}

	h.clock.Advance(56 * time.Minute)
	if c := decodeJSON[[]authCheck](t, h.mustRun("accounts", "check", "gmail:work"))[0]; c.AccessToken != "expiring" {
		t.Errorf("4 minutes before expiry access_token = %q, want expiring", c.AccessToken)
	}
	h.clock.Advance(time.Hour)
	if c := decodeJSON[[]authCheck](t, h.mustRun("accounts", "check", "gmail:work"))[0]; c.AccessToken != "expired" {
		t.Errorf("after expiry access_token = %q, want expired", c.AccessToken)
	}
	h.wantError(h.run("accounts", "check", "gmail:ghost"), ExitNeedsConnect, "not-connected")
}

func TestAuthCheckWithNoAccounts(t *testing.T) {
	h := newHarness(t)
	if r := h.mustRun("accounts", "check"); strings.TrimSpace(r.stdout) != "[]" {
		t.Errorf("stdout = %q, want []", r.stdout)
	}
}

func openRawDB(t *testing.T, home string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "addition.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func auditEvents(t *testing.T, home string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "addition.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT event FROM audit_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var e string
		_ = rows.Scan(&e)
		events = append(events, e)
	}
	return events
}

// dumpDB concatenates every text value in the vault, so tests can look for plaintext secrets.
func dumpDB(t *testing.T, home string) string {
	t.Helper()
	db := openRawDB(t, home)
	var all strings.Builder
	for _, table := range []string{"meta", "oauth_clients", "accounts", "credentials", "audit_log", "pending_auth"} {
		rows, err := db.Query(`SELECT * FROM ` + table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			_ = rows.Scan(ptrs...)
			for _, v := range vals {
				if s, ok := v.(string); ok {
					all.WriteString(s + "\n")
				}
			}
		}
		_ = rows.Close()
	}
	return all.String()
}

func assertAuditHoldsNoSecrets(t *testing.T, h *harness) {
	t.Helper()
	var audit strings.Builder
	rows, err := openRawDB(t, h.home).Query(`SELECT event || COALESCE(provider,'') || details FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		audit.WriteString(s)
	}
	_ = rows.Close()
	all := dumpDB(t, h.home)
	for i, secret := range append([]string{fakeClientSecret, fakeAccessToken, fakeRefreshToken, workIdentity, refreshedAccessPrefix, rotatedRefreshPrefix}, h.google.verifiers...) {
		if secret == "" {
			continue
		}
		if strings.Contains(audit.String(), secret) {
			t.Errorf("the audit log holds secret #%d", i)
		}
		if secret != workIdentity && strings.Contains(all, secret) {
			t.Errorf("the vault holds secret #%d in plaintext", i)
		}
	}
}
