package vault_test

import (
	"context"
	"testing"

	"github.com/runagent-dev/addition/internal/vault"
)

func TestSnapshotReadsClientsAccountsAndCredentialsInOneTransaction(t *testing.T) {
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
	// Another connection changes the client and the account together, between the snapshot's reads.
	restore := vault.SetAfterSnapshotClients(func() {
		tx, err := rawDB(t, f.home).Begin()
		if err != nil {
			return
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.Exec(`DELETE FROM oauth_clients`); err != nil {
			return
		}
		if _, err := tx.Exec(`UPDATE accounts SET status = 'needs-reauth'`); err != nil {
			return
		}
		_ = tx.Commit()
	})
	defer restore()

	snap, err := s.Snapshot(ctx)

	if err != nil {
		t.Fatal(err)
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
}

func TestReadOnlyOpenRefusesAVaultWithSideFiles(t *testing.T) {
	s, f := newStore(t)
	_ = s.Close()
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			path := f.home + "/addition.db" + suffix
			if err := writeEmptyPrivateFile(path); err != nil {
				t.Fatal(err)
			}
			defer removeFile(path)

			_, err := vault.OpenWith(context.Background(), f.env(), vault.OpenOptions{ReadOnly: true})

			if !isErr(err, vault.ErrInUse) {
				t.Errorf("OpenWith err = %v, want ErrInUse", err)
			}
		})
	}
}
