package vault_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/crypto"
	"github.com/runagent-dev/addition/internal/vault"
)

const (
	clientSecret = "GOCSPX-client-secret-plaintext"
	accessToken  = "ya29.access-token-plaintext"
	refreshToken = "1//refresh-token-plaintext"
)

func TestOAuthClients(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t)

	if _, err := s.GetOAuthClient(ctx, "google"); !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("Get missing client err = %v, want ErrNotFound", err)
	}

	google := vault.OAuthClient{Provider: "google", ClientID: "id-1.apps.googleusercontent.com", ClientSecret: clientSecret}
	if err := s.PutOAuthClient(ctx, google); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.PutOAuthClient(ctx, vault.OAuthClient{Provider: "github", ClientID: "gh-id", ClientSecret: "gh-secret"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.GetOAuthClient(ctx, "google")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ClientID != google.ClientID || got.ClientSecret != clientSecret {
		t.Errorf("Get = %+v, want %+v", got, google)
	}
	if !got.CreatedAt.Equal(testNow) || !got.UpdatedAt.Equal(testNow) {
		t.Errorf("timestamps = %v/%v, want the injected clock", got.CreatedAt, got.UpdatedAt)
	}

	google.ClientID, google.ClientSecret = "id-2", "rotated-secret"
	if err := s.PutOAuthClient(ctx, google); err != nil {
		t.Fatalf("Put (replace): %v", err)
	}
	got, _ = s.GetOAuthClient(ctx, "google")
	if got.ClientID != "id-2" || got.ClientSecret != "rotated-secret" {
		t.Errorf("Put did not replace the client: %+v", got)
	}

	list, err := s.ListOAuthClients(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var providers []string
	for _, c := range list {
		providers = append(providers, c.Provider)
	}
	if !reflect.DeepEqual(providers, []string{"github", "google"}) {
		t.Errorf("List providers = %v, want [github google]", providers)
	}

	assertNotStoredInPlaintext(t, f.home, clientSecret, "rotated-secret", "gh-secret")
}

func TestAccounts(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)

	work, err := s.CreateAccount(ctx, vault.Account{
		Provider: "google", Alias: "work", Identity: "me@work.com", Subject: "1001",
		Scopes: []string{"openid", "https://www.googleapis.com/auth/gmail.readonly"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if work.ID == 0 || work.Status != vault.AccountActive || !work.CreatedAt.Equal(testNow) {
		t.Errorf("Create = %+v, want an ID, status active and the injected clock", work)
	}

	if _, err := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work"}); !errors.Is(err, vault.ErrAliasTaken) {
		t.Errorf("duplicate alias err = %v, want ErrAliasTaken", err)
	}
	if _, err := s.CreateAccount(ctx, vault.Account{Provider: "github", Alias: "work"}); err != nil {
		t.Errorf("same alias, other provider: %v", err)
	}
	if _, err := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "personal"}); err != nil {
		t.Fatalf("Create personal: %v", err)
	}

	got, err := s.GetAccount(ctx, "google", "work")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, work) {
		t.Errorf("Get = %+v, want %+v", got, work)
	}
	if _, err := s.GetAccount(ctx, "google", "nope"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("Get missing err = %v, want ErrNotFound", err)
	}

	assertAliases(t, s, "google", "personal", "work")
	assertAliases(t, s, "", "work", "personal", "work")

	if err := s.RemoveAccount(ctx, "google", "work"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := s.RemoveAccount(ctx, "google", "work"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("Remove twice err = %v, want ErrNotFound", err)
	}
	assertAliases(t, s, "google", "personal")
}

func assertAliases(t *testing.T, s *vault.Store, provider string, want ...string) {
	t.Helper()
	list, err := s.ListAccounts(context.Background(), provider)
	if err != nil {
		t.Fatalf("List(%q): %v", provider, err)
	}
	var got []string
	for _, a := range list {
		got = append(got, a.Alias)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List(%q) aliases = %v, want %v (ordered by provider, alias)", provider, got, want)
	}
}

func TestCredentials(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t)
	acct, err := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetCredential(ctx, acct.ID); !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("Get missing err = %v, want ErrNotFound", err)
	}

	expiry := testNow.Add(time.Hour)
	want := vault.Credential{AccessToken: accessToken, RefreshToken: refreshToken, TokenType: "Bearer", ExpiresAt: expiry}
	if err := s.PutCredential(ctx, acct.ID, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.GetCredential(ctx, acct.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AccessToken != accessToken || got.RefreshToken != refreshToken || got.TokenType != "Bearer" || !got.ExpiresAt.Equal(expiry) {
		t.Errorf("Get = %+v, want %+v", got, want)
	}
	assertNotStoredInPlaintext(t, f.home, accessToken, refreshToken)

	if err := s.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: "second", TokenType: "Bearer"}); err != nil {
		t.Fatalf("Put (replace): %v", err)
	}
	got, _ = s.GetCredential(ctx, acct.ID)
	if got.AccessToken != "second" || got.RefreshToken != "" || !got.ExpiresAt.IsZero() {
		t.Errorf("replaced credential = %+v, want no refresh token and no expiry", got)
	}

	if err := s.PutCredential(ctx, 9999, want); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("Put for missing account err = %v, want ErrNotFound", err)
	}

	if err := s.RemoveAccount(ctx, "google", "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCredential(ctx, acct.ID); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("credential survived account removal (err = %v)", err)
	}
}

type auditRow struct {
	event, provider string
	accountID       int64
	details, at     string
}

func auditRows(t *testing.T, home string) []auditRow {
	t.Helper()
	rows, err := rawDB(t, home).Query(`SELECT event, COALESCE(provider,''), COALESCE(account_id,0), details, created_at FROM audit_log WHERE event != 'init' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.event, &r.provider, &r.accountID, &r.details, &r.at); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	return got
}

func TestAudit(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t)
	acct, _ := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work"})

	events := []vault.AuditEvent{
		vault.ConnectEvent{AccountID: acct.ID},
		vault.RefreshEvent{AccountID: acct.ID, Outcome: vault.RefreshNeedsReauth},
		vault.RevokeEvent{AccountID: acct.ID},
	}
	for _, ev := range events {
		if err := s.Audit(ctx, ev); err != nil {
			t.Fatalf("Audit(%T): %v", ev, err)
		}
	}
	_ = s.RemoveAccount(ctx, "google", "work")

	got := auditRows(t, f.home)
	want := []auditRow{
		{"connect", "google", acct.ID, "{}", ""},
		{"refresh", "google", acct.ID, `{"outcome":"needs-reauth"}`, ""},
		{"revoke", "google", acct.ID, "{}", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("audit rows = %+v, want %d", got, len(want))
	}
	for i := range want {
		want[i].at = got[i].at
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v (kept after the account was removed)", i, got[i], want[i])
		}
	}
	if at, err := time.Parse(time.RFC3339Nano, got[0].at); err != nil || !at.Equal(testNow) {
		t.Errorf("created_at = %q, want the injected clock", got[0].at)
	}
}

func TestAuditRejectsUnknownValues(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t)
	acct, _ := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work"})

	bad := map[string]vault.AuditEvent{
		"unknown outcome":    vault.RefreshEvent{AccountID: acct.ID, Outcome: "ok-ish"},
		"unknown key source": vault.InitEvent{KeySource: "paper"},
		"missing account":    vault.ConnectEvent{AccountID: 9999},
		"no account":         vault.RevokeEvent{},
	}
	for name, ev := range bad {
		if err := s.Audit(ctx, ev); err == nil {
			t.Errorf("%s: Audit accepted it", name)
		}
	}
	if rows := auditRows(t, f.home); len(rows) != 0 {
		t.Errorf("rejected events were written: %+v", rows)
	}
}

// Every caller-controlled string an audit entry point accepts carries the marker.
func TestAuditNeverPersistsCallerStrings(t *testing.T) {
	const marker = "ya29.SYNTHETIC-TOKEN-MARKER"
	ctx := context.Background()
	s, f := newStore(t)
	acct, _ := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work"})

	attempts := []vault.AuditEvent{
		vault.InitEvent{KeySource: marker},
		vault.ConnectEvent{AccountID: acct.ID},
		vault.RefreshEvent{AccountID: acct.ID, Outcome: marker},
		vault.RefreshEvent{AccountID: acct.ID, Outcome: vault.RefreshOK},
		vault.RevokeEvent{AccountID: acct.ID},
	}
	for _, ev := range attempts {
		_ = s.Audit(ctx, ev)
		_ = s.WithImmediateTx(ctx, func(tx *vault.Tx) error { return tx.Audit(ctx, ev) })
	}

	for _, a := range []vault.Account{{Provider: marker, Alias: "work"}, {Provider: "google", Alias: marker}} {
		if _, err := s.CreateAccount(ctx, a); !errors.Is(err, vault.ErrInvalidAccount) {
			t.Errorf("CreateAccount(%+v) err = %v, want ErrInvalidAccount", a, err)
		}
	}

	if n := auditRowsContaining(t, f.home, marker); n != 0 {
		t.Errorf("%d audit rows contain the marker", n)
	}
	if strings.Contains(dumpAllText(t, f.home), marker) {
		t.Error("the marker reached the database")
	}
}

func auditRowsContaining(t *testing.T, home, needle string) int {
	t.Helper()
	var n int
	if err := rawDB(t, home).QueryRow(`SELECT count(*) FROM audit_log WHERE instr(event || COALESCE(provider,'') || COALESCE(account_id,'') || details || created_at, ?) > 0`, needle).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAuditNeverStoresTheAlias(t *testing.T) {
	const alias = "alias-canary-7"
	ctx := context.Background()
	s, f := newStore(t)
	acct, err := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: alias})
	if err != nil {
		t.Fatal(err)
	}

	err = s.WithImmediateTx(ctx, func(tx *vault.Tx) error {
		for _, ev := range []vault.AuditEvent{vault.ConnectEvent{AccountID: acct.ID}, vault.RevokeEvent{AccountID: acct.ID}} {
			if err := tx.Audit(ctx, ev); err != nil {
				return err
			}
		}
		return tx.RemoveAccount(ctx, "google", alias)
	})
	if err != nil {
		t.Fatal(err)
	}

	rows := auditRows(t, f.home)
	if len(rows) != 2 || rows[1].accountID != acct.ID || rows[1].provider != "google" {
		t.Errorf("audit rows = %+v, want connect and revoke keyed by account %d", rows, acct.ID)
	}
	if n := auditRowsContaining(t, f.home, alias); n != 0 {
		t.Errorf("%d audit rows contain the alias", n)
	}
}

func TestCreateAccountValidatesProviderAndAlias(t *testing.T) {
	const marker = "ya29.SYNTHETIC-TOKEN-MARKER"
	ctx := context.Background()
	s, f := newStore(t)
	long := strings.Repeat("a", 33)

	bad := []vault.Account{
		{Provider: marker, Alias: "work"},
		{Provider: "google", Alias: marker},
		{Provider: "", Alias: "work"},
		{Provider: "Google", Alias: "work"},
		{Provider: "9lives", Alias: "work"},
		{Provider: "google-", Alias: "work"},
		{Provider: "goo--gle", Alias: "work"},
		{Provider: "google", Alias: ""},
		{Provider: "google", Alias: "-work"},
		{Provider: "google", Alias: "Work"},
		{Provider: "google", Alias: "work mail"},
		{Provider: "google", Alias: long},
	}
	for _, a := range bad {
		_, err := s.CreateAccount(ctx, a)
		if !errors.Is(err, vault.ErrInvalidAccount) {
			t.Errorf("CreateAccount(%q, %q) err = %v, want ErrInvalidAccount", a.Provider, a.Alias, err)
		}
		if err != nil && strings.Contains(err.Error(), marker) {
			t.Errorf("error echoes the rejected value: %v", err)
		}
	}
	good := []vault.Account{
		{Provider: "google", Alias: "work"},
		{Provider: "google-workspace", Alias: "0_personal-2"},
		{Provider: "x1", Alias: long[:32]},
	}
	for _, a := range good {
		if _, err := s.CreateAccount(ctx, a); err != nil {
			t.Errorf("CreateAccount(%q, %q): %v", a.Provider, a.Alias, err)
		}
	}
	if strings.Contains(dumpAllText(t, f.home), marker) {
		t.Error("the marker reached the database")
	}
}

func TestSecretsAreBoundToTheirRowAndField(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		swap  string
		check func(s *vault.Store, a, b vault.Account) error
	}{
		"access token copied to another account": {
			`UPDATE credentials SET access_token_encrypted = (SELECT access_token_encrypted FROM credentials WHERE account_id = :a) WHERE account_id = :b`,
			func(s *vault.Store, _, b vault.Account) error { _, err := s.GetCredential(ctx, b.ID); return err },
		},
		"refresh token copied into the access column": {
			`UPDATE credentials SET access_token_encrypted = refresh_token_encrypted WHERE account_id = :a`,
			func(s *vault.Store, a, _ vault.Account) error { _, err := s.GetCredential(ctx, a.ID); return err },
		},
		"access token copied into the refresh column": {
			`UPDATE credentials SET refresh_token_encrypted = access_token_encrypted WHERE account_id = :a`,
			func(s *vault.Store, a, _ vault.Account) error { _, err := s.GetCredential(ctx, a.ID); return err },
		},
		"client secret copied to another provider": {
			`UPDATE oauth_clients SET client_secret_encrypted = (SELECT client_secret_encrypted FROM oauth_clients WHERE provider = 'google') WHERE provider = 'github'`,
			func(s *vault.Store, _, _ vault.Account) error { _, err := s.GetOAuthClient(ctx, "github"); return err },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, f := newStore(t)
			a, _ := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "a"})
			b, _ := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "b"})
			for _, acct := range []vault.Account{a, b} {
				if err := s.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: "access-" + acct.Alias, RefreshToken: "refresh-" + acct.Alias}); err != nil {
					t.Fatal(err)
				}
			}
			for _, p := range []string{"google", "github"} {
				if err := s.PutOAuthClient(ctx, vault.OAuthClient{Provider: p, ClientID: p, ClientSecret: "secret-" + p}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := rawDB(t, f.home).Exec(tc.swap, sql.Named("a", a.ID), sql.Named("b", b.ID)); err != nil {
				t.Fatal(err)
			}

			if err := tc.check(s, a, b); !errors.Is(err, crypto.ErrDecrypt) {
				t.Errorf("err = %v, want ErrDecrypt for a moved blob", err)
			}
		})
	}
}

func TestAccountIDsAreNeverReused(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	first, _ := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work"})
	_ = s.RemoveAccount(ctx, "google", "work")

	second, err := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID <= first.ID {
		t.Errorf("recreated account got id %d, want one above %d", second.ID, first.ID)
	}
}

func TestInitWritesAnAuditRow(t *testing.T) {
	_, f := newStore(t)
	var n int
	if err := rawDB(t, f.home).QueryRow(`SELECT count(*) FROM audit_log WHERE event='init'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("init audit rows = %d (err %v), want 1", n, err)
	}
}

func assertNotStoredInPlaintext(t *testing.T, home string, secrets ...string) {
	t.Helper()
	dump := dumpAllText(t, home)
	for _, secret := range secrets {
		if strings.Contains(dump, secret) {
			t.Errorf("%q is stored in plaintext", secret)
		}
	}
}

func TestAuditCallEvent(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t)
	acct, _ := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work"})

	for _, ev := range []vault.CallEvent{
		{AccountID: acct.ID, Method: "GET", Status: 200},
		{AccountID: acct.ID, Method: "POST", Status: 0},
	} {
		if err := s.Audit(ctx, ev); err != nil {
			t.Fatalf("Audit(%+v): %v", ev, err)
		}
	}

	rows := auditRows(t, f.home)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %+v, want 2", rows)
	}
	if rows[0].event != "call" || rows[0].provider != "google" || rows[0].accountID != acct.ID || rows[0].details != `{"method":"GET","status":"200"}` {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[1].details != `{"method":"POST","status":"0"}` {
		t.Errorf("a call with no response records status 0, got %s", rows[1].details)
	}
}

func TestAuditRejectsMalformedCallEvents(t *testing.T) {
	const marker = "ya29.SYNTHETIC-TOKEN-MARKER"
	ctx := context.Background()
	s, f := newStore(t)
	acct, _ := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work"})

	bad := map[string]vault.CallEvent{
		"lowercase method":  {AccountID: acct.ID, Method: "get", Status: 200},
		"free-text method":  {AccountID: acct.ID, Method: marker, Status: 200},
		"unknown method":    {AccountID: acct.ID, Method: "TRACE", Status: 200},
		"status too low":    {AccountID: acct.ID, Method: "GET", Status: 99},
		"status too high":   {AccountID: acct.ID, Method: "GET", Status: 600},
		"negative status":   {AccountID: acct.ID, Method: "GET", Status: -1},
		"no account":        {Method: "GET", Status: 200},
		"unknown account":   {AccountID: 9999, Method: "GET", Status: 200},
		"empty method":      {AccountID: acct.ID, Status: 200},
		"method with space": {AccountID: acct.ID, Method: "GET /x", Status: 200},
	}
	for name, ev := range bad {
		if err := s.Audit(ctx, ev); err == nil {
			t.Errorf("%s: Audit accepted it", name)
		}
	}
	if rows := auditRows(t, f.home); len(rows) != 0 {
		t.Errorf("rejected events were written: %+v", rows)
	}
}

func TestAccountNeedsReauthStatusRoundTrips(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	acct, _ := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work"})

	acct.Status = vault.AccountNeedsReauth
	if _, err := s.UpdateAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}

	got, _ := s.GetAccount(ctx, "google", "work")
	if got.Status != "needs-reauth" {
		t.Errorf("status = %q, want needs-reauth", got.Status)
	}
}
