// Package vault is addn's local store: one SQLite file whose secrets are
// sealed with a master key that never touches the database.
package vault

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/runagent-dev/addition/internal/crypto"
)

var (
	ErrNotInitialized      = errors.New("no vault here yet; run `addn init`")
	ErrKeychainUnavailable = errors.New("the OS keychain is unavailable")
	ErrNoMasterKey         = errors.New("no master key available")
	ErrWrongMasterKey      = errors.New("the master key does not match this vault")
	ErrInsecureKeyFile     = errors.New("the key file is readable by group or others")
	ErrSchemaTooNew        = errors.New("the vault was written by a newer addn")
	ErrNotFound            = errors.New("not found")
	ErrAliasTaken          = errors.New("alias already in use for this provider")
)

type InitOptions struct {
	// KeyFile keeps the master key in $ADDN_HOME/master.key (0600) instead of the OS keychain.
	KeyFile bool
}

type InitResult struct {
	Home          string    `json:"home"`
	KeySource     KeySource `json:"key_source"`
	SchemaVersion int       `json:"schema_version"`
	Created       bool      `json:"created"`
}

// Init creates the vault, or verifies an existing one; it never replaces a key.
func Init(ctx context.Context, env Env, opts InitOptions) (InitResult, error) {
	env = env.withDefaults()
	home, err := ResolveHome(env)
	if err != nil {
		return InitResult{}, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return InitResult{}, fmt.Errorf("create data dir: %w", err)
	}
	db, err := openDB(ctx, home)
	if err != nil {
		return InitResult{}, err
	}
	defer db.Close()
	if err := os.Chmod(filepath.Join(home, dbFile), 0o600); err != nil {
		return InitResult{}, err
	}

	result := InitResult{Home: home, SchemaVersion: SchemaVersion}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return InitResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	recorded, err := getMeta(ctx, tx, metaKeySource)
	switch {
	case err == nil:
		if _, err := unlock(ctx, tx, env, home); err != nil {
			return InitResult{}, err
		}
		result.KeySource = KeySource(recorded)
		return result, nil
	case !errors.Is(err, ErrNotFound):
		return InitResult{}, err
	}

	key, source, err := provisionKey(env, home, opts.KeyFile)
	if err != nil {
		return InitResult{}, err
	}
	canary, err := newCanary(key)
	if err != nil {
		return InitResult{}, err
	}
	if err := setMeta(ctx, tx, metaKeySource, string(source)); err != nil {
		return InitResult{}, err
	}
	if err := setMeta(ctx, tx, metaKeyCanary, canary); err != nil {
		return InitResult{}, err
	}
	ops := ops{q: tx, now: env.Now}
	if err := ops.Audit(ctx, "init", nil, map[string]any{"key_source": source}); err != nil {
		return InitResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return InitResult{}, err
	}
	result.KeySource, result.Created = source, true
	return result, nil
}

// Store is an open vault. All methods take and return plaintext; secrets are sealed at this boundary.
type Store struct {
	ops
	db *sql.DB
}

// Open unlocks an initialised vault, applying any pending migrations.
func Open(ctx context.Context, env Env) (*Store, error) {
	env = env.withDefaults()
	home, err := ResolveHome(env)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(home, dbFile)); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w (looked in %s)", ErrNotInitialized, home)
	}
	db, err := openDB(ctx, home)
	if err != nil {
		return nil, err
	}
	key, err := unlock(ctx, db, env, home)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{ops: ops{q: db, key: key, now: env.Now}, db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// unlock resolves the master key and proves it against the stored canary.
func unlock(ctx context.Context, q querier, env Env, home string) (crypto.Key, error) {
	recorded, err := getMeta(ctx, q, metaKeySource)
	if errors.Is(err, ErrNotFound) {
		return crypto.Key{}, fmt.Errorf("%w (looked in %s)", ErrNotInitialized, home)
	}
	if err != nil {
		return crypto.Key{}, err
	}
	canary, err := getMeta(ctx, q, metaKeyCanary)
	if err != nil {
		return crypto.Key{}, fmt.Errorf("read key canary: %w", err)
	}
	key, err := resolveKey(env, home, KeySource(recorded))
	if err != nil {
		return crypto.Key{}, err
	}
	if err := checkCanary(key, canary); err != nil {
		return crypto.Key{}, err
	}
	return key, nil
}
