package vault

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrInvalidAuditEvent rejects an event before anything is written; it never echoes the bad value.
var ErrInvalidAuditEvent = errors.New("invalid audit event")

// AuditEvent is one of the event types in this file. Their fields are the only details
// an audit row can hold: enums checked against a fixed set, and account IDs.
type AuditEvent interface {
	auditEntry() (auditEntry, error)
}

type auditEntry struct {
	event     string
	accountID int64 // 0 for events with no account
	details   map[string]string
}

type InitEvent struct{ KeySource KeySource }

func (e InitEvent) auditEntry() (auditEntry, error) {
	if !e.KeySource.valid() {
		return auditEntry{}, fmt.Errorf("%w: unknown key source", ErrInvalidAuditEvent)
	}
	return auditEntry{event: "init", details: map[string]string{"key_source": string(e.KeySource)}}, nil
}

type ConnectEvent struct{ AccountID int64 }

func (e ConnectEvent) auditEntry() (auditEntry, error) {
	return accountEntry("connect", e.AccountID, nil)
}

type RefreshOutcome string

const (
	RefreshOK          RefreshOutcome = "ok"
	RefreshNeedsReauth RefreshOutcome = "needs-reauth"
	RefreshFailed      RefreshOutcome = "failed"
)

type RefreshEvent struct {
	AccountID int64
	Outcome   RefreshOutcome
}

func (e RefreshEvent) auditEntry() (auditEntry, error) {
	switch e.Outcome {
	case RefreshOK, RefreshNeedsReauth, RefreshFailed:
	default:
		return auditEntry{}, fmt.Errorf("%w: unknown refresh outcome", ErrInvalidAuditEvent)
	}
	return accountEntry("refresh", e.AccountID, map[string]string{"outcome": string(e.Outcome)})
}

// RevokeEvent must be written before the account is removed.
type RevokeEvent struct{ AccountID int64 }

func (e RevokeEvent) auditEntry() (auditEntry, error) {
	return accountEntry("revoke", e.AccountID, nil)
}

func accountEntry(event string, accountID int64, details map[string]string) (auditEntry, error) {
	if accountID <= 0 {
		return auditEntry{}, fmt.Errorf("%w: %s needs an account", ErrInvalidAuditEvent, event)
	}
	return auditEntry{event: event, accountID: accountID, details: details}, nil
}

// Audit records ev. Provider and alias are copied from the accounts table so history survives removal.
func (o ops) Audit(ctx context.Context, ev AuditEvent) error {
	entry, err := ev.auditEntry()
	if err != nil {
		return err
	}
	var id sql.NullInt64
	var provider, alias sql.NullString
	if entry.accountID != 0 {
		err := o.q.QueryRowContext(ctx, `SELECT id, provider, alias FROM accounts WHERE id = ?`, entry.accountID).Scan(&id, &provider, &alias)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("audit %s: account %d: %w", entry.event, entry.accountID, ErrNotFound)
		}
		if err != nil {
			return err
		}
	}
	if entry.details == nil {
		entry.details = map[string]string{}
	}
	details, err := json.Marshal(entry.details)
	if err != nil {
		return err
	}
	_, err = o.q.ExecContext(ctx, `
		INSERT INTO audit_log (event, account_id, provider, alias, details, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		entry.event, id, provider, alias, string(details), o.timestamp())
	return err
}
