package vault

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrPendingExpired = errors.New("the pending authorization has expired")

// PendingAuth is an authorization request waiting for its redirect. An empty Verifier means no PKCE.
type PendingAuth struct {
	State       string
	Provider    string
	Alias       string
	Scopes      []string
	RedirectURI string
	Verifier    string
	ExpiresAt   time.Time
}

const sortableTime = "2006-01-02T15:04:05.000000000Z"

func formatSortable(t time.Time) string { return t.UTC().Format(sortableTime) }

func verifierSlot(state string) slot {
	return slot{"pending_auth", state, "code_verifier_encrypted"}
}

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
	_, err = o.q.ExecContext(ctx, `
		INSERT INTO pending_auth (state, provider, alias, scopes, redirect_uri, code_verifier_encrypted, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.State, p.Provider, p.Alias, string(scopes), p.RedirectURI, verifier, o.timestamp(), formatSortable(p.ExpiresAt))
	return err
}

// TakePendingAuth deletes and returns the authorization for state, so it is usable once,
// and prunes every expired one. An expired match is ErrPendingExpired; no match is ErrNotFound.
func (o ops) TakePendingAuth(ctx context.Context, state string) (PendingAuth, error) {
	p := PendingAuth{State: state}
	var scopes, expires string
	var verifier sql.NullString
	err := o.q.QueryRowContext(ctx, `
		DELETE FROM pending_auth WHERE state = ?
		RETURNING provider, alias, scopes, redirect_uri, code_verifier_encrypted, expires_at`, state).
		Scan(&p.Provider, &p.Alias, &scopes, &p.RedirectURI, &verifier, &expires)
	if pruneErr := o.pruneExpiredPendingAuth(ctx); pruneErr != nil && err == nil {
		err = pruneErr
	}
	if errors.Is(err, sql.ErrNoRows) {
		return PendingAuth{}, fmt.Errorf("pending authorization: %w", ErrNotFound)
	}
	if err != nil {
		return PendingAuth{}, err
	}
	if p.ExpiresAt, err = time.Parse(sortableTime, expires); err != nil {
		return PendingAuth{}, err
	}
	if !o.now().Before(p.ExpiresAt) {
		return PendingAuth{}, ErrPendingExpired
	}
	if err := json.Unmarshal([]byte(scopes), &p.Scopes); err != nil {
		return PendingAuth{}, fmt.Errorf("pending authorization scopes: %w", err)
	}
	if verifier.Valid {
		if p.Verifier, err = o.open(verifierSlot(state), verifier.String); err != nil {
			return PendingAuth{}, fmt.Errorf("pending authorization: %w", err)
		}
	}
	return p, nil
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
