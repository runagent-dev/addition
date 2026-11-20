package vault

import "context"

// Tx is a write transaction holding the database lock; it offers the same operations as Store.
type Tx struct {
	ops
}

// WithImmediateTx runs fn inside BEGIN IMMEDIATE, so concurrent writers, in this or
// other processes, run one at a time. fn's error rolls the transaction back.
func (s *Store) WithImmediateTx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := beginWriterTx(ctx, s.db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(&Tx{ops{q: tx, key: s.key, vaultID: s.vaultID, now: s.now}}); err != nil {
		return err
	}
	return tx.Commit()
}
