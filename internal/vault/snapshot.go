package vault

import (
	"context"
	"errors"
)

// Snapshot is the vault data a report needs, read in one transaction so it never mixes two states.
type Snapshot struct {
	KeySource   KeySource
	Clients     []OAuthClient
	Accounts    []Account
	Credentials map[int64]Credential
}

// afterSnapshotClients lets tests commit a change between the reads of a Snapshot.
var afterSnapshotClients = func() {}

// Snapshot reads the key source, OAuth clients, accounts and credentials in one deferred read
// transaction: a writer that commits meanwhile is either wholly in the snapshot or wholly out of it.
func (s *Store) Snapshot(ctx context.Context) (Snapshot, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN DEFERRED`); err != nil {
		return Snapshot{}, err
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`) }()
	reader := s.ops
	reader.q = conn

	snap := Snapshot{Credentials: map[int64]Credential{}}
	if snap.Clients, err = reader.ListOAuthClients(ctx); err != nil {
		return Snapshot{}, err
	}
	afterSnapshotClients()
	if snap.Accounts, err = reader.ListAccounts(ctx, ""); err != nil {
		return Snapshot{}, err
	}
	for _, acct := range snap.Accounts {
		cred, err := reader.GetCredential(ctx, acct.ID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return Snapshot{}, err
		}
		snap.Credentials[acct.ID] = cred
	}
	source, err := getMeta(ctx, conn, metaKeySource)
	if err != nil {
		return Snapshot{}, err
	}
	snap.KeySource = KeySource(source)
	return snap, nil
}
