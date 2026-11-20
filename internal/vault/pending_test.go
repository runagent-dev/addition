package vault_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/vault"
)

const pkceVerifier = "pkce-verifier-plaintext-0123456789abcdefghijk"

func pending(state string, expires time.Time) vault.PendingAuth {
	return vault.PendingAuth{
		State:       state,
		Provider:    "google",
		Alias:       "work",
		Scopes:      []string{"openid", "email"},
		RedirectURI: "http://127.0.0.1:4567/callback",
		Verifier:    pkceVerifier,
		ExpiresAt:   expires,
	}
}

func TestPendingAuthIsTakenOnce(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t)
	want := pending("state-1", testNow.Add(10*time.Minute))
	if err := s.PutPendingAuth(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if strings.Contains(dumpAllText(t, f.home), pkceVerifier) {
		t.Error("the PKCE verifier is stored in plaintext")
	}

	got, err := s.TakePendingAuth(ctx, "state-1")
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, want.ExpiresAt)
	}
	got.ExpiresAt = want.ExpiresAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Take = %+v, want %+v", got, want)
	}
	if _, err := s.TakePendingAuth(ctx, "state-1"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("second Take err = %v, want ErrNotFound", err)
	}
}

func TestPendingAuthWithoutPKCE(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	p := pending("state-nopkce", testNow.Add(time.Minute))
	p.Verifier = ""
	if err := s.PutPendingAuth(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := s.TakePendingAuth(ctx, "state-nopkce")
	if err != nil || got.Verifier != "" {
		t.Errorf("Take = %+v, %v; want an empty verifier", got, err)
	}
}

func TestPendingAuthUnknownState(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.TakePendingAuth(context.Background(), "never-issued"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestExpiredPendingAuthIsRejectedAndPruned(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t)
	for _, p := range []vault.PendingAuth{
		pending("expired", testNow.Add(-time.Second)),
		pending("also-expired", testNow.Add(-time.Hour)),
		pending("live", testNow.Add(time.Minute)),
	} {
		if err := s.PutPendingAuth(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.TakePendingAuth(ctx, "expired"); !errors.Is(err, vault.ErrPendingExpired) {
		t.Errorf("err = %v, want ErrPendingExpired", err)
	}
	var states []string
	rows, err := rawDB(t, f.home).Query(`SELECT state FROM pending_auth ORDER BY state`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		states = append(states, s)
	}
	_ = rows.Close()
	if !reflect.DeepEqual(states, []string{"live"}) {
		t.Errorf("remaining pending states = %v, want only the live one", states)
	}
}

func TestPendingAuthRejectsDuplicateState(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	p := pending("dup", testNow.Add(time.Minute))
	if err := s.PutPendingAuth(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPendingAuth(ctx, p); err == nil {
		t.Error("a second pending authorization with the same state was accepted")
	}
}

func TestUpdateAccount(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	acct, err := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work", Scopes: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}

	acct.Identity, acct.Scopes, acct.Status = "me@work.example", []string{"a", "b"}, vault.AccountActive
	updated, err := s.UpdateAccount(ctx, acct)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := s.GetAccount(ctx, "google", "work")
	if got.ID != acct.ID || got.Identity != "me@work.example" || !reflect.DeepEqual(got.Scopes, []string{"a", "b"}) {
		t.Errorf("after update = %+v", got)
	}
	if !updated.UpdatedAt.Equal(testNow) {
		t.Errorf("updated_at = %v", updated.UpdatedAt)
	}

	ghost := vault.Account{Provider: "google", Alias: "ghost", Status: vault.AccountActive}
	if _, err := s.UpdateAccount(ctx, ghost); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("Update missing err = %v, want ErrNotFound", err)
	}
}

func TestDefaultAccount(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	if _, err := s.DefaultAccount(ctx, "google"); !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("no default err = %v, want ErrNotFound", err)
	}
	if err := s.SetDefaultAccount(ctx, "google", "work"); !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("default for a missing account err = %v, want ErrNotFound", err)
	}
	for _, alias := range []string{"work", "personal"} {
		if _, err := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: alias}); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.SetDefaultAccount(ctx, "google", "work"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.DefaultAccount(ctx, "google"); got != "work" {
		t.Errorf("default = %q, want work", got)
	}
	if err := s.RemoveAccount(ctx, "google", "personal"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.DefaultAccount(ctx, "google"); got != "work" {
		t.Errorf("removing another account changed the default to %q", got)
	}
	if err := s.RemoveAccount(ctx, "google", "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DefaultAccount(ctx, "google"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("default after removing it err = %v, want ErrNotFound", err)
	}
}

func TestOpenUpgradesASchemaOneVault(t *testing.T) {
	s, f := newStore(t)
	_ = s.Close()
	db := rawDB(t, f.home)
	if _, err := db.Exec(`DROP TABLE pending_auth; UPDATE meta SET value = '1' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}

	s = mustOpen(t, f)
	if err := s.PutPendingAuth(context.Background(), pending("after-upgrade", testNow.Add(time.Minute))); err != nil {
		t.Errorf("pending_auth unusable after upgrade: %v", err)
	}
	var version string
	_ = db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version)
	if version != "2" {
		t.Errorf("schema_version = %s, want 2", version)
	}
}
