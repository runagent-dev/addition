package vault_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/runagent-dev/addition/internal/vault"
)

func TestACredentialRecordsHowItWasObtained(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	oauth, _ := s.CreateAccount(ctx, vault.Account{Provider: "gmail", Alias: "work"})
	token, _ := s.CreateAccount(ctx, vault.Account{Provider: "github", Alias: "me"})

	if err := s.PutCredential(ctx, oauth.ID, vault.Credential{AccessToken: accessToken, RefreshToken: refreshToken}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCredential(ctx, token.ID, vault.Credential{AccessToken: accessToken, Kind: vault.KindAPIKey}); err != nil {
		t.Fatal(err)
	}

	gotOAuth, _ := s.GetCredential(ctx, oauth.ID)
	gotToken, err := s.GetCredential(ctx, token.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOAuth.Kind != vault.KindOAuth2 {
		t.Errorf("a credential stored without a kind is %q, want oauth2", gotOAuth.Kind)
	}
	if gotToken.Kind != vault.KindAPIKey || gotToken.AccessToken != accessToken || gotToken.RefreshToken != "" || !gotToken.ExpiresAt.IsZero() {
		t.Errorf("api key credential = %+v", gotToken)
	}
	if vault.KindOAuth2 != "oauth2" || vault.KindAPIKey != "api_key" {
		t.Errorf("kinds = %q, %q; they are stored values", vault.KindOAuth2, vault.KindAPIKey)
	}
}

func TestReplacingACredentialReplacesItsKindAndBumpsTheRevision(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	acct, _ := s.CreateAccount(ctx, vault.Account{Provider: "github", Alias: "me"})
	_ = s.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: accessToken, RefreshToken: refreshToken})

	if err := s.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: "ghp_second", Kind: vault.KindAPIKey}); err != nil {
		t.Fatal(err)
	}

	got, _ := s.GetCredential(ctx, acct.ID)
	if got.Kind != vault.KindAPIKey || got.Revision != 2 || got.AccessToken != "ghp_second" || got.RefreshToken != "" {
		t.Errorf("credential = %+v, want a second api_key revision with no refresh token", got)
	}
}

func TestAnUnknownKindIsRefusedWithoutEchoingIt(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	acct, _ := s.CreateAccount(ctx, vault.Account{Provider: "github", Alias: "me"})

	err := s.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: accessToken, Kind: "bearer-ish"})

	if !errors.Is(err, vault.ErrInvalidCredential) {
		t.Fatalf("err = %v, want ErrInvalidCredential", err)
	}
	if strings.Contains(err.Error(), "bearer-ish") || strings.Contains(err.Error(), accessToken) {
		t.Errorf("the error repeats its input: %v", err)
	}
	if _, err := s.GetCredential(ctx, acct.ID); !errors.Is(err, vault.ErrNotFound) {
		t.Errorf("a refused credential was stored: %v", err)
	}
}

func TestAnAPIKeyIsSealedLikeEveryOtherToken(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t)
	acct, _ := s.CreateAccount(ctx, vault.Account{Provider: "github", Alias: "me"})
	if err := s.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: accessToken, Kind: vault.KindAPIKey}); err != nil {
		t.Fatal(err)
	}

	if dump := dumpAllText(t, f.home); strings.Contains(dump, accessToken) {
		t.Error("the api key is in the database in plaintext")
	}
	var blob string
	if err := rawDB(t, f.home).QueryRow(`SELECT access_token_encrypted FROM credentials WHERE account_id = ?`, acct.ID).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	other, _ := s.CreateAccount(ctx, vault.Account{Provider: "github", Alias: "other"})
	if _, err := rawDB(t, f.home).Exec(`INSERT INTO credentials (account_id, access_token_encrypted, updated_at, kind) VALUES (?, ?, 'x', 'api_key')`, other.ID, blob); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCredential(ctx, other.ID); err == nil {
		t.Error("a blob copied to another account opened; its AAD must still bind the row")
	}
}

func TestOpenUpgradesASchemaThreeVaultKeepingItsOAuthCredentials(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t)
	acct, _ := s.CreateAccount(ctx, vault.Account{Provider: "gmail", Alias: "work"})
	_ = s.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: accessToken, RefreshToken: refreshToken})
	_ = s.Close()
	if _, err := rawDB(t, f.home).Exec(`ALTER TABLE credentials DROP COLUMN kind; UPDATE meta SET value = '3' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}

	s = mustOpen(t, f)

	got, err := s.GetCredential(ctx, acct.ID)
	if err != nil {
		t.Fatalf("credential after upgrade: %v", err)
	}
	if got.Kind != vault.KindOAuth2 || got.AccessToken != accessToken || got.RefreshToken != refreshToken || got.Revision != 1 {
		t.Errorf("credential after upgrade = %+v, want the same oauth2 tokens at revision 1", got)
	}
	var version string
	_ = rawDB(t, f.home).QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version)
	if version != "4" {
		t.Errorf("schema_version = %s, want 4", version)
	}
}

func TestAnOldBinaryStillRefusesTheNewerSchema(t *testing.T) {
	s, f := newStore(t)
	_ = s.Close()
	if _, err := rawDB(t, f.home).Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`, vault.SchemaVersion+1); err != nil {
		t.Fatal(err)
	}

	if err := openErr(t, f); !errors.Is(err, vault.ErrSchemaTooNew) {
		t.Errorf("Open err = %v, want ErrSchemaTooNew", err)
	}
}
