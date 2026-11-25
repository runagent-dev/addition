package proxy

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/oauth"
	"github.com/runagent-dev/addition/internal/vault"
)

// AccessTokenSkew is how long before its expiry an access token is refreshed.
const AccessTokenSkew = 60 * time.Second

// refreshTimeout bounds a token exchange that cancellation no longer interrupts.
const refreshTimeout = 30 * time.Second

// TokenManager hands out a usable access token for one account and refreshes it under the
// vault's writer lock, so concurrent addition processes refresh each rotation once.
type TokenManager struct {
	Store    *vault.Store
	Client   *http.Client
	Provider *catalog.Provider
	Account  vault.Account
	Now      func() time.Time
	// BeforeWriterLock, if set, runs once the credential has been read and the writer lock is about to be requested.
	BeforeWriterLock func()
	// BeforeExchange, if set, runs once the writer lock is held and the token endpoint is about to be called.
	BeforeExchange func()

	mu        sync.Mutex
	handedOut *seen
	method    string
}

// AuthMethod is how the credential last handed out was obtained: catalog.MethodOAuth2 or MethodAPIKey.
func (m *TokenManager) AuthMethod() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.method
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
	if acct.ID != m.Account.ID {
		return "", ErrAccountChanged
	}
	if acct.Status != vault.AccountActive {
		return "", reauthError(ctx, m.Store, acct.ID)
	}
	cred, err := m.Store.GetCredential(ctx, acct.ID)
	if errors.Is(err, vault.ErrNotFound) {
		return "", ErrNoCredential
	}
	if err != nil {
		return "", err
	}
	m.remember(cred)
	if cred.Kind == vault.KindAPIKey || !m.expiring(cred) {
		return cred.AccessToken, nil
	}
	return m.refresh(ctx, seen{revision: cred.Revision, haveRevision: true})
}

// Renew replaces an access token the API rejected. If another process committed a refresh since
// this one read the credential, that token is returned without a second refresh.
func (m *TokenManager) Renew(ctx context.Context, rejected string) (string, error) {
	return m.refresh(ctx, m.lastSeen(rejected))
}

// seen is what a caller knew of the credential before asking for a refresh: its revision, or,
// for a caller that never read it, only the text of the token the API rejected.
type seen struct {
	revision     int64
	haveRevision bool
	rejected     string
}

func (s seen) isStale(c vault.Credential) bool {
	if s.haveRevision {
		return c.Revision == s.revision
	}
	return c.AccessToken == s.rejected
}

func (m *TokenManager) remember(c vault.Credential) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handedOut = &seen{revision: c.Revision, haveRevision: true}
	m.method = c.Kind
}

// reauthError says the account must be reconnected: with a new token if its credential is an API key.
func reauthError(ctx context.Context, store credentialReader, accountID int64) error {
	if cred, err := store.GetCredential(ctx, accountID); err == nil && cred.Kind == vault.KindAPIKey {
		return ErrTokenRejected
	}
	return ErrNeedsReauth
}

type credentialReader interface {
	GetCredential(ctx context.Context, accountID int64) (vault.Credential, error)
}

func (m *TokenManager) lastSeen(rejected string) seen {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handedOut != nil {
		return *m.handedOut
	}
	return seen{rejected: rejected}
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
// Once the exchange starts the provider may have rotated the grant, so it and its commit finish
// whatever ctx says, and a cancellation is reported only afterwards.
func (m *TokenManager) refresh(ctx context.Context, caller seen) (string, error) {
	var latest vault.Credential
	var failure error
	exchanged := false
	if m.BeforeWriterLock != nil {
		m.BeforeWriterLock()
	}
	err := m.Store.WithImmediateTx(ctx, func(tx *vault.Tx) error {
		acct, err := tx.GetAccount(ctx, m.Account.Provider, m.Account.Alias)
		if errors.Is(err, vault.ErrNotFound) {
			failure = ErrNoCredential
			return nil
		}
		if err != nil {
			return err
		}
		if acct.ID != m.Account.ID {
			failure = ErrAccountChanged
			return nil
		}
		if acct.Status != vault.AccountActive {
			failure = reauthError(ctx, tx, acct.ID)
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
		if !caller.isStale(cred) {
			latest = cred
			return nil
		}
		if cred.Kind == vault.KindAPIKey {
			failure = ErrTokenRejected
			acct.Status = vault.AccountNeedsReauth
			_, err := tx.UpdateAccount(ctx, acct)
			return err
		}
		if cred.RefreshToken == "" {
			failure = ErrNoRefreshToken
			return nil
		}
		client, err := tx.GetOAuthClient(ctx, m.Provider.IdentityRoot)
		if errors.Is(err, vault.ErrNotFound) {
			failure = ErrNoOAuthClient
			return nil
		}
		if err != nil {
			return err
		}
		if m.BeforeExchange != nil {
			m.BeforeExchange()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		exchanged = true
		settle := tx.Settle(ctx)
		exchangeCtx, stop := context.WithTimeout(settle, refreshTimeout)
		defer stop()
		fresh, err := oauth.Refresh(exchangeCtx, m.Client, m.Provider, oauth.Client{ID: client.ClientID, Secret: client.ClientSecret}, cred.RefreshToken)
		if err != nil {
			failure = err
			if isDeadGrant(err) {
				failure = ErrNeedsReauth
			}
			return recordRefreshFailure(settle, tx, acct, isDeadGrant(err))
		}
		if err := storeRefreshed(settle, tx, acct, cred, fresh, m.now()); err != nil {
			return err
		}
		latest, err = tx.GetCredential(settle, acct.ID)
		return err
	})
	if err != nil {
		return "", err
	}
	if cancelled := ctx.Err(); exchanged && cancelled != nil {
		return "", cancelled
	}
	if failure != nil {
		return "", failure
	}
	m.remember(latest)
	return latest.AccessToken, nil
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
