package vault_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

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
		Provider: "google", Alias: "work", Identity: "me@work.com",
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

func TestAudit(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t)
	acct, _ := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work"})

	if err := s.Audit(ctx, "connect", &acct, map[string]any{"scopes": []string{"gmail"}}); err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if err := s.Audit(ctx, "client.add", nil, nil); err != nil {
		t.Fatalf("Audit without account: %v", err)
	}
	_ = s.RemoveAccount(ctx, "google", "work")

	rows, err := rawDB(t, f.home).Query(`SELECT event, COALESCE(provider,''), COALESCE(alias,''), details, created_at FROM audit_log WHERE event IN ('connect','client.add') ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct{ event, provider, alias, details, at string }
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.event, &r.provider, &r.alias, &r.details, &r.at); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("audit rows = %+v, want 2", got)
	}
	if got[0].event != "connect" || got[0].provider != "google" || got[0].alias != "work" {
		t.Errorf("connect row = %+v, want google/work kept after the account was removed", got[0])
	}
	var details map[string][]string
	if err := json.Unmarshal([]byte(got[0].details), &details); err != nil || details["scopes"][0] != "gmail" {
		t.Errorf("details = %q, want JSON with scopes", got[0].details)
	}
	if at, err := time.Parse(time.RFC3339Nano, got[0].at); err != nil || !at.Equal(testNow) {
		t.Errorf("created_at = %q, want the injected clock", got[0].at)
	}
	if got[1].provider != "" || got[1].alias != "" || got[1].details != "{}" {
		t.Errorf("account-less row = %+v, want no account and {} details", got[1])
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
