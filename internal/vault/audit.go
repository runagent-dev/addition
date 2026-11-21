package vault

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
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

// CallMethods are the HTTP methods a CallEvent may record.
var CallMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

// CallEvent is one finished API request. Status is the final HTTP status, or 0 when none arrived.
// It never records the path, query, headers or body.
type CallEvent struct {
	AccountID int64
	Method    string
	Status    int
}

func (e CallEvent) auditEntry() (auditEntry, error) {
	if !slices.Contains(CallMethods, e.Method) {
		return auditEntry{}, fmt.Errorf("%w: unknown call method", ErrInvalidAuditEvent)
	}
	if e.Status != 0 && (e.Status < 100 || e.Status > 599) {
		return auditEntry{}, fmt.Errorf("%w: call status out of range", ErrInvalidAuditEvent)
	}
	return accountEntry("call", e.AccountID, map[string]string{"method": e.Method, "status": strconv.Itoa(e.Status)})
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

// Audit records ev. A row names its account by opaque id and provider id only, so it survives removal.
func (o ops) Audit(ctx context.Context, ev AuditEvent) error {
	entry, err := ev.auditEntry()
	if err != nil {
		return err
	}
	var accountID sql.NullInt64
	var provider sql.NullString
	if entry.accountID != 0 {
		err := o.q.QueryRowContext(ctx, `SELECT id, provider FROM accounts WHERE id = ?`, entry.accountID).Scan(&accountID, &provider)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("audit %s: account %d: %w", entry.event, entry.accountID, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if !providerIDPattern.MatchString(provider.String) {
			return fmt.Errorf("%w: account %d has a malformed provider id", ErrInvalidAuditEvent, entry.accountID)
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
		INSERT INTO audit_log (event, account_id, provider, details, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		entry.event, accountID, provider, string(details), o.timestamp())
	return err
}

// AuditWithinDeadline is Audit for a caller whose ctx has a deadline: a writer lock held by another
// connection fails it when that deadline passes, not after the connection's own longer busy wait.
func (s *Store) AuditWithinDeadline(ctx context.Context, ev AuditEvent) error {
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return s.Audit(ctx, ev)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := setBusyTimeout(ctx, conn, max(time.Until(deadline), 0)); err != nil {
		return err
	}
	defer func() { _ = setBusyTimeout(context.WithoutCancel(ctx), conn, busyTimeout) }()
	return ops{q: conn, key: s.key, vaultID: s.vaultID, now: s.now}.Audit(ctx, ev)
}

func setBusyTimeout(ctx context.Context, conn *sql.Conn, wait time.Duration) error {
	_, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", wait.Milliseconds()))
	return err
}
