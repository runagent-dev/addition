package vault

import "context"

// Tx is a write transaction holding the database lock; it offers the same operations as Store.
type Tx struct {
	ops
	settled bool
}

// Settle declares that the work from here on must finish even if ctx is cancelled, because it records
// something already done outside the database. It returns the context to run the remaining statements
// with, and the transaction then commits whatever ctx says.
func (t *Tx) Settle(ctx context.Context) context.Context {
	t.settled = true
	return context.WithoutCancel(ctx)
}

// WithImmediateTx runs fn inside BEGIN IMMEDIATE, so concurrent writers, in this or
// other processes, run one at a time. fn's error or ctx's cancellation rolls it back, unless fn settled it.
func (s *Store) WithImmediateTx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := beginInterruptibleWriterTx(ctx, s.db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	handle := &Tx{ops: ops{q: tx, key: s.key, vaultID: s.vaultID, now: s.now}}
	if err := fn(handle); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil && !handle.settled {
		return err
	}
	return tx.Commit()
}
