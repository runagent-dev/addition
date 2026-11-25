package vault_test

import (
	"context"
	"testing"

	"github.com/runagent-dev/addition/internal/vault"
)

func TestEveryCredentialWriteBumpsItsRevision(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	acct, err := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work"})
	if err != nil {
		t.Fatal(err)
	}

	var revisions []int64
	for range 3 {
		if err := s.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: accessToken}); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetCredential(ctx, acct.ID)
		if err != nil {
			t.Fatal(err)
		}
		revisions = append(revisions, got.Revision)
	}

	if revisions[0] != 1 || revisions[1] != 2 || revisions[2] != 3 {
		t.Errorf("revisions = %v, want 1, 2, 3 even though the token text never changed", revisions)
	}
}

func TestOpenUpgradesASchemaTwoVaultWithRevisionOnePerCredential(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t)
	acct, _ := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work"})
	_ = s.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: accessToken})
	_ = s.Close()
	db := rawDB(t, f.home)
	if _, err := db.Exec(`ALTER TABLE credentials DROP COLUMN revision; ALTER TABLE credentials DROP COLUMN kind; UPDATE meta SET value = '2' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}

	s = mustOpen(t, f)

	got, err := s.GetCredential(ctx, acct.ID)
	if err != nil || got.Revision != 1 || got.AccessToken != accessToken {
		t.Fatalf("credential after upgrade = %+v, %v; want revision 1 and the same token", got, err)
	}
	_ = s.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: accessToken})
	if got, _ := s.GetCredential(ctx, acct.ID); got.Revision != 2 {
		t.Errorf("revision after a write = %d, want 2", got.Revision)
	}
}
