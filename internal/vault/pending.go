package vault

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrPendingExpired = errors.New("the pending authorization has expired")
	ErrPendingClaimed = errors.New("another process is completing this pending authorization")
)

// PendingAuth is an authorization request waiting for its redirect. AccountScopes is what the
// alias held when it started. An empty Verifier means no PKCE.
type PendingAuth struct {
	State         string
	Provider      string
	Alias         string
	Scopes        []string
	AccountScopes []string
	RedirectURI   string
	Verifier      string
	ExpiresAt     time.Time
}

const sortableTime = "2006-01-02T15:04:05.000000000Z"

func formatSortable(t time.Time) string { return t.UTC().Format(sortableTime) }

func verifierSlot(state string) slot {
	return slot{"pending_auth", state, "code_verifier_encrypted"}
}

// PutPendingAuth stores p and prunes every expired authorization.
func (o ops) PutPendingAuth(ctx context.Context, p PendingAuth) error {
	if err := ValidateAccountName(p.Provider, p.Alias); err != nil {
		return err
	}
	if p.State == "" {
		return errors.New("a pending authorization needs a state")
	}
	var verifier sql.NullString
	if p.Verifier != "" {
		blob, err := o.seal(verifierSlot(p.State), p.Verifier)
		if err != nil {
			return err
		}
		verifier = sql.NullString{String: blob, Valid: true}
	}
	scopes, err := json.Marshal(nonNil(p.Scopes))
	if err != nil {
		return err
	}
	accountScopes, err := json.Marshal(nonNil(p.AccountScopes))
	if err != nil {
		return err
	}
	if err := o.pruneExpiredPendingAuth(ctx); err != nil {
		return err
	}
	_, err = o.q.ExecContext(ctx, `
		INSERT INTO pending_auth (state, provider, alias, scopes, account_scopes, redirect_uri, code_verifier_encrypted, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.State, p.Provider, p.Alias, string(scopes), string(accountScopes), p.RedirectURI, verifier,
		o.timestamp(), formatSortable(p.ExpiresAt))
	return err
}

// GetPendingAuth returns the live authorization for state without claiming it, and prunes
// every expired one. An expired match is ErrPendingExpired; no match is ErrNotFound.
func (s *Store) GetPendingAuth(ctx context.Context, state string) (PendingAuth, error) {
	return s.findPendingAuth(ctx, state, false)
}

// ClaimPendingAuth is GetPendingAuth that also marks the authorization as being completed,
// so only one process exchanges its code; a claimed one is ErrPendingClaimed.
func (s *Store) ClaimPendingAuth(ctx context.Context, state string) (PendingAuth, error) {
	return s.findPendingAuth(ctx, state, true)
}

func (s *Store) findPendingAuth(ctx context.Context, state string, claim bool) (PendingAuth, error) {
	var p PendingAuth
	var found error
	err := s.WithImmediateTx(ctx, func(tx *Tx) error {
		var claimed bool
		p, claimed, found = tx.readPendingAuth(ctx, state)
		if err := tx.pruneExpiredPendingAuth(ctx); err != nil {
			return err
		}
		switch {
		case found != nil:
			return nil
		case !tx.now().Before(p.ExpiresAt):
			found = ErrPendingExpired
			return nil
		case claim && claimed:
			found = ErrPendingClaimed
			return nil
		case claim:
			_, err := tx.q.ExecContext(ctx, `UPDATE pending_auth SET claimed_at = ? WHERE state = ?`, tx.timestamp(), state)
			return err
		}
		return nil
	})
	if err != nil {
		return PendingAuth{}, err
	}
	if found != nil {
		return PendingAuth{}, found
	}
	return p, nil
}

func (o ops) readPendingAuth(ctx context.Context, state string) (PendingAuth, bool, error) {
	p := PendingAuth{State: state}
	var scopes, accountScopes, expires string
	var verifier, claimedAt sql.NullString
	err := o.q.QueryRowContext(ctx, `
		SELECT provider, alias, scopes, account_scopes, redirect_uri, code_verifier_encrypted, claimed_at, expires_at
		FROM pending_auth WHERE state = ?`, state).
		Scan(&p.Provider, &p.Alias, &scopes, &accountScopes, &p.RedirectURI, &verifier, &claimedAt, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingAuth{}, false, fmt.Errorf("pending authorization: %w", ErrNotFound)
	}
	if err != nil {
		return PendingAuth{}, false, err
	}
	if p.ExpiresAt, err = time.Parse(sortableTime, expires); err != nil {
		return PendingAuth{}, false, err
	}
	if err := json.Unmarshal([]byte(scopes), &p.Scopes); err != nil {
		return PendingAuth{}, false, fmt.Errorf("pending authorization scopes: %w", err)
	}
	if err := json.Unmarshal([]byte(accountScopes), &p.AccountScopes); err != nil {
		return PendingAuth{}, false, fmt.Errorf("pending authorization account scopes: %w", err)
	}
	if verifier.Valid {
		if p.Verifier, err = o.open(verifierSlot(state), verifier.String); err != nil {
			return PendingAuth{}, false, fmt.Errorf("pending authorization: %w", err)
		}
	}
	return p, claimedAt.Valid, nil
}

// ReleasePendingAuth gives up a claim, so the authorization can be completed again.
func (o ops) ReleasePendingAuth(ctx context.Context, state string) error {
	_, err := o.q.ExecContext(ctx, `UPDATE pending_auth SET claimed_at = NULL WHERE state = ?`, state)
	return err
}

// DeletePendingAuth removes the authorization for state, if it is still there.
func (o ops) DeletePendingAuth(ctx context.Context, state string) error {
	_, err := o.q.ExecContext(ctx, `DELETE FROM pending_auth WHERE state = ?`, state)
	return err
}

func (o ops) pruneExpiredPendingAuth(ctx context.Context) error {
	_, err := o.q.ExecContext(ctx, `DELETE FROM pending_auth WHERE expires_at <= ?`, formatSortable(o.now()))
	return err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
