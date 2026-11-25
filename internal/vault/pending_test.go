package vault_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/vault"
)

const pkceVerifier = "pkce-verifier-plaintext-0123456789abcdefghijk"

func pending(state string, expires time.Time) vault.PendingAuth {
	return vault.PendingAuth{
		State:         state,
		Provider:      "google",
		Alias:         "work",
		Scopes:        []string{"openid", "email"},
		AccountScopes: []string{"openid"},
		RedirectURI:   "http://127.0.0.1:4567/callback",
		Verifier:      pkceVerifier,
		ExpiresAt:     expires,
	}
}

func TestPendingAuthRoundtrip(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t)
	want := pending("state-1", testNow.Add(10*time.Minute))
	if err := s.PutPendingAuth(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if strings.Contains(dumpAllText(t, f.home), pkceVerifier) {
		t.Error("the PKCE verifier is stored in plaintext")
	}

	for _, get := range []func(context.Context, string) (vault.PendingAuth, error){s.GetPendingAuth, s.ClaimPendingAuth} {
		got, err := get(ctx, "state-1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !got.ExpiresAt.Equal(want.ExpiresAt) {
			t.Errorf("expires_at = %v, want %v", got.ExpiresAt, want.ExpiresAt)
		}
		got.ExpiresAt = want.ExpiresAt
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	}
}

func TestPendingAuthIsClaimedOnce(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	if err := s.PutPendingAuth(ctx, pending("state-1", testNow.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimPendingAuth(ctx, "state-1"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := s.ClaimPendingAuth(ctx, "state-1"); !errors.Is(err, vault.ErrPendingClaimed) {
		t.Errorf("second claim err = %v, want ErrPendingClaimed", err)
	}
	if _, err := s.GetPendingAuth(ctx, "state-1"); err != nil {
		t.Errorf("a claimed authorization is no longer readable: %v", err)
	}

	if err := s.ReleasePendingAuth(ctx, "state-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimPendingAuth(ctx, "state-1"); err != nil {
		t.Errorf("claim after release: %v", err)
	}

	if err := s.DeletePendingAuth(ctx, "state-1"); err != nil {
		t.Fatal(err)
	}
	for name, get := range map[string]func(context.Context, string) (vault.PendingAuth, error){"get": s.GetPendingAuth, "claim": s.ClaimPendingAuth} {
		if _, err := get(ctx, "state-1"); !errors.Is(err, vault.ErrNotFound) {
			t.Errorf("%s after delete err = %v, want ErrNotFound", name, err)
		}
	}
}

func TestConcurrentClaimsHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t)
	if err := s.PutPendingAuth(ctx, pending("contested", testNow.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	const claimers = 8
	errs := make([]error, claimers)
	var wg sync.WaitGroup
	for i := range claimers {
		wg.Go(func() {
			other := mustOpen(t, f)
			_, errs[i] = other.ClaimPendingAuth(ctx, "contested")
		})
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, vault.ErrPendingClaimed):
			t.Errorf("claim err = %v, want nil or ErrPendingClaimed", err)
		}
	}
	if won != 1 {
		t.Errorf("%d claims won, want 1", won)
	}
}

func TestDeleteUnclaimedPendingAuth(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	for _, state := range []string{"unclaimed", "claimed"} {
		if err := s.PutPendingAuth(ctx, pending(state, testNow.Add(time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ClaimPendingAuth(ctx, "claimed"); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteUnclaimedPendingAuth(ctx, "unclaimed"); err != nil {
		t.Errorf("unclaimed: %v", err)
	}
	if _, err := s.GetPendingAuth(ctx, "unclaimed"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("after delete, get = %v, want ErrNotFound", err)
	}
	if err := s.DeleteUnclaimedPendingAuth(ctx, "claimed"); !errors.Is(err, vault.ErrPendingClaimed) {
		t.Errorf("claimed: %v, want ErrPendingClaimed", err)
	}
	if _, err := s.GetPendingAuth(ctx, "claimed"); err != nil {
		t.Errorf("a claimed authorization was removed: %v", err)
	}
	if err := s.DeleteUnclaimedPendingAuth(ctx, "never-issued"); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("unknown: %v, want ErrNotFound", err)
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
	got, err := s.ClaimPendingAuth(ctx, "state-nopkce")
	if err != nil || got.Verifier != "" {
		t.Errorf("Claim = %+v, %v; want an empty verifier", got, err)
	}
}

func TestPendingAuthUnknownState(t *testing.T) {
	s, _ := newStore(t)
	for name, get := range map[string]func(context.Context, string) (vault.PendingAuth, error){"get": s.GetPendingAuth, "claim": s.ClaimPendingAuth} {
		if _, err := get(context.Background(), "never-issued"); !errors.Is(err, vault.ErrNotFound) {
			t.Errorf("%s err = %v, want ErrNotFound", name, err)
		}
	}
}

func pendingStates(t *testing.T, home string) []string {
	t.Helper()
	var states []string
	rows, err := rawDB(t, home).Query(`SELECT state FROM pending_auth ORDER BY state`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		states = append(states, s)
	}
	return states
}

// insertRawPending writes a pending row directly, bypassing the prune that PutPendingAuth runs.
func insertRawPending(t *testing.T, home, state string, expires time.Time) {
	t.Helper()
	_, err := rawDB(t, home).Exec(`INSERT INTO pending_auth (state, provider, alias, redirect_uri, created_at, expires_at)
		VALUES (?, 'google', 'work', 'http://127.0.0.1:1/callback', '', ?)`, state, expires.UTC().Format("2006-01-02T15:04:05.000000000Z"))
	if err != nil {
		t.Fatal(err)
	}
}

func TestExpiredPendingAuthIsRejectedAndPruned(t *testing.T) {
	for name, get := range map[string]func(*vault.Store) func(context.Context, string) (vault.PendingAuth, error){
		"get": func(s *vault.Store) func(context.Context, string) (vault.PendingAuth, error) { return s.GetPendingAuth },
		"claim": func(s *vault.Store) func(context.Context, string) (vault.PendingAuth, error) {
			return s.ClaimPendingAuth
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s, f := newStore(t)
			if err := s.PutPendingAuth(ctx, pending("live", testNow.Add(time.Minute))); err != nil {
				t.Fatal(err)
			}
			insertRawPending(t, f.home, "expired", testNow.Add(-time.Second))
			insertRawPending(t, f.home, "also-expired", testNow.Add(-time.Hour))
			if _, err := get(s)(ctx, "expired"); !errors.Is(err, vault.ErrPendingExpired) {
				t.Errorf("err = %v, want ErrPendingExpired", err)
			}
			if states := pendingStates(t, f.home); !reflect.DeepEqual(states, []string{"live"}) {
				t.Errorf("remaining pending states = %v, want only the live one", states)
			}
		})
	}
}

func TestPutPendingAuthPrunesExpiredRows(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t)
	insertRawPending(t, f.home, "at-the-boundary", testNow)
	insertRawPending(t, f.home, "just-live", testNow.Add(time.Nanosecond))
	if err := s.PutPendingAuth(ctx, pending("next", testNow.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if states := pendingStates(t, f.home); !reflect.DeepEqual(states, []string{"just-live", "next"}) {
		t.Errorf("pending states = %v, want the expired one pruned", states)
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

	acct.Identity, acct.Subject, acct.Scopes, acct.Status = "me@work.example", "1001", []string{"a", "b"}, vault.AccountActive
	updated, err := s.UpdateAccount(ctx, acct)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := s.GetAccount(ctx, "google", "work")
	if got.ID != acct.ID || got.Identity != "me@work.example" || got.Subject != "1001" || !reflect.DeepEqual(got.Scopes, []string{"a", "b"}) {
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
	if _, err := db.Exec(`DROP TABLE pending_auth; ALTER TABLE accounts DROP COLUMN subject; ALTER TABLE credentials DROP COLUMN revision; ALTER TABLE credentials DROP COLUMN kind;
		UPDATE meta SET value = '1' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}

	s = mustOpen(t, f)
	if err := s.PutPendingAuth(context.Background(), pending("after-upgrade", testNow.Add(time.Minute))); err != nil {
		t.Errorf("pending_auth unusable after upgrade: %v", err)
	}
	if _, err := s.CreateAccount(context.Background(), vault.Account{Provider: "google", Alias: "work", Subject: "1001"}); err != nil {
		t.Errorf("accounts.subject unusable after upgrade: %v", err)
	}
	var version string
	_ = db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version)
	if version != "4" {
		t.Errorf("schema_version = %s, want 4", version)
	}
}
