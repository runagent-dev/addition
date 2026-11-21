package vault_test

import (
	"context"
	"testing"

	"github.com/runagent-dev/addition/internal/vault"
)

func TestSnapshotNeverMixesStatesWhenAWriterCommitsAndCheckpointsBetweenReads(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t)
	if err := s.PutOAuthClient(ctx, vault.OAuthClient{Provider: "google", ClientID: "123456789012-x.apps.googleusercontent.com", ClientSecret: "s3cret"}); err != nil {
		t.Fatal(err)
	}
	acct, err := s.CreateAccount(ctx, vault.Account{Provider: "gmail", Alias: "work", Subject: "1", Status: vault.AccountActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	writerCommitted := false
	restore := vault.SetAfterSnapshotClients(func() {
		db := rawDB(t, f.home)
		tx, err := db.Begin()
		if err != nil {
			t.Errorf("the writer could not begin: %v", err)
			return
		}
		if _, err := tx.Exec(`DELETE FROM oauth_clients`); err != nil {
			t.Errorf("delete clients: %v", err)
		}
		if _, err := tx.Exec(`UPDATE accounts SET status = 'needs-reauth'`); err != nil {
			t.Errorf("update account: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Errorf("the writer could not commit: %v", err)
			return
		}
		_, _ = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		writerCommitted = true
	})
	defer restore()

	snap, err := s.Snapshot(ctx)

	if err != nil {
		t.Fatal(err)
	}
	if !writerCommitted {
		t.Fatal("the concurrent writer never committed, so the test proves nothing")
	}
	if len(snap.Accounts) != 1 {
		t.Fatalf("accounts = %+v", snap.Accounts)
	}
	clientPresent, reauth := len(snap.Clients) == 1, snap.Accounts[0].Status == vault.AccountNeedsReauth
	if clientPresent == reauth {
		t.Errorf("client present = %v, account needs-reauth = %v: the snapshot mixes two states", clientPresent, reauth)
	}
	if cred := snap.Credentials[acct.ID]; cred.RefreshToken != "r" {
		t.Errorf("credential = %+v", cred)
	}
	if after, err := s.Snapshot(ctx); err != nil || len(after.Clients) != 0 || after.Accounts[0].Status != vault.AccountNeedsReauth {
		t.Errorf("a fresh snapshot = %+v, %v; want the writer's state", after, err)
	}
}

func TestSnapshotNamesTheRecordedKeySource(t *testing.T) {
	s, _ := newStore(t)

	snap, err := s.Snapshot(context.Background())

	if err != nil || snap.KeySource != vault.KeySourceFile {
		t.Errorf("snapshot key source = %q, %v; want %q", snap.KeySource, err, vault.KeySourceFile)
	}
}
