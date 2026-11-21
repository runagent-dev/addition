package proxy

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/oauth"
	"github.com/runagent-dev/addition/internal/vault"
)

// AccessTokenSkew is how long before its expiry an access token is refreshed.
const AccessTokenSkew = 60 * time.Second

// TokenManager hands out a usable access token for one account and refreshes it under the
// vault's writer lock, so concurrent addn processes refresh each rotation once.
type TokenManager struct {
	Store    *vault.Store
	Client   *http.Client
	Provider *catalog.Provider
	Account  vault.Account
	Now      func() time.Time
}

// Access returns the account's access token, refreshing it first if it expires within AccessTokenSkew.
func (m *TokenManager) Access(ctx context.Context) (string, error) {
	acct, err := m.Store.GetAccount(ctx, m.Account.Provider, m.Account.Alias)
	if errors.Is(err, vault.ErrNotFound) {
		return "", ErrNoCredential
	}
	if err != nil {
		return "", err
	}
	if acct.Status != vault.AccountActive {
		return "", ErrNeedsReauth
	}
	cred, err := m.Store.GetCredential(ctx, acct.ID)
	if errors.Is(err, vault.ErrNotFound) {
		return "", ErrNoCredential
	}
	if err != nil {
		return "", err
	}
	if !m.expiring(cred) {
		return cred.AccessToken, nil
	}
	return m.refresh(ctx, m.expiring)
}

// Renew replaces an access token the API rejected. If another process already stored a
// different token, that one is returned without a refresh.
func (m *TokenManager) Renew(ctx context.Context, rejected string) (string, error) {
	return m.refresh(ctx, func(c vault.Credential) bool { return c.AccessToken == rejected })
}

func (m *TokenManager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *TokenManager) expiring(c vault.Credential) bool {
	return !c.ExpiresAt.IsZero() && !m.now().Add(AccessTokenSkew).Before(c.ExpiresAt)
}

// refresh re-reads the credential inside the writer transaction and, only if stale still holds,
// trades its refresh token for new tokens. A refusal is committed as audit and account state.
func (m *TokenManager) refresh(ctx context.Context, stale func(vault.Credential) bool) (string, error) {
	var token string
	var failure error
	err := m.Store.WithImmediateTx(ctx, func(tx *vault.Tx) error {
		acct, err := tx.GetAccount(ctx, m.Account.Provider, m.Account.Alias)
		if errors.Is(err, vault.ErrNotFound) || err == nil && acct.ID != m.Account.ID {
			failure = ErrNoCredential
			return nil
		}
		if err != nil {
			return err
		}
		if acct.Status != vault.AccountActive {
			failure = ErrNeedsReauth
			return nil
		}
		cred, err := tx.GetCredential(ctx, acct.ID)
		if errors.Is(err, vault.ErrNotFound) {
			failure = ErrNoCredential
			return nil
		}
		if err != nil {
			return err
		}
		if !stale(cred) {
			token = cred.AccessToken
			return nil
		}
		if cred.RefreshToken == "" {
			failure = ErrNoRefreshToken
			return nil
		}
		client, err := tx.GetOAuthClient(ctx, m.Provider.ID)
		if errors.Is(err, vault.ErrNotFound) {
			failure = ErrNoOAuthClient
			return nil
		}
		if err != nil {
			return err
		}
		fresh, err := oauth.Refresh(ctx, m.Client, m.Provider, oauth.Client{ID: client.ClientID, Secret: client.ClientSecret}, cred.RefreshToken)
		if err != nil {
			failure = err
			if isDeadGrant(err) {
				failure = ErrNeedsReauth
			}
			return recordRefreshFailure(ctx, tx, acct, isDeadGrant(err))
		}
		token = fresh.AccessToken
		return storeRefreshed(ctx, tx, acct, cred, fresh, m.now())
	})
	if err != nil {
		return "", err
	}
	return token, failure
}

func storeRefreshed(ctx context.Context, tx *vault.Tx, acct vault.Account, old vault.Credential, fresh oauth.Tokens, now time.Time) error {
	next := vault.Credential{AccessToken: fresh.AccessToken, RefreshToken: old.RefreshToken, TokenType: old.TokenType}
	if fresh.RefreshToken != "" {
		next.RefreshToken = fresh.RefreshToken
	}
	if fresh.TokenType != "" {
		next.TokenType = fresh.TokenType
	}
	if fresh.ExpiresIn > 0 {
		next.ExpiresAt = now.Add(fresh.ExpiresIn)
	}
	if err := tx.PutCredential(ctx, acct.ID, next); err != nil {
		return err
	}
	return tx.Audit(ctx, vault.RefreshEvent{AccountID: acct.ID, Outcome: vault.RefreshOK})
}

// isDeadGrant is the provider saying the refresh token is revoked, expired or otherwise unusable.
func isDeadGrant(err error) bool {
	var refused *oauth.ProviderError
	return errors.As(err, &refused) && refused.Code == "invalid_grant"
}

// recordRefreshFailure commits a refused refresh: a dead grant marks the account needs-reauth,
// anything else leaves it active. It returns nil once recorded, so the transaction commits.
func recordRefreshFailure(ctx context.Context, tx *vault.Tx, acct vault.Account, deadGrant bool) error {
	outcome := vault.RefreshFailed
	if deadGrant {
		outcome = vault.RefreshNeedsReauth
		acct.Status = vault.AccountNeedsReauth
		if _, err := tx.UpdateAccount(ctx, acct); err != nil {
			return err
		}
	}
	return tx.Audit(ctx, vault.RefreshEvent{AccountID: acct.ID, Outcome: outcome})
}
