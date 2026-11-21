// Package vault is addn's local store: one SQLite file whose secrets are
// sealed with a master key that never touches the database.
package vault

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/runagent-dev/addition/internal/crypto"
)

var (
	ErrNotInitialized      = errors.New("no vault here yet; run `addn init`")
	ErrKeychainUnavailable = errors.New("the OS keychain is unavailable")
	ErrNoMasterKey         = errors.New("no master key available")
	ErrWrongMasterKey      = errors.New("the master key does not match this vault")
	ErrInsecureKeyFile     = errors.New("the key file is not a private regular file")
	ErrInsecureHome        = errors.New("the data dir is not private to this user")
	ErrKeyFileUnsupported  = errors.New("key files are not supported on Windows yet; use the Windows Credential Manager (the default keychain source) or set ADDN_MASTER_KEY")
	ErrSchemaTooNew        = errors.New("the vault was written by a newer addn")
	ErrCorrupt             = errors.New("the vault database is corrupt")
	ErrNotFound            = errors.New("not found")
	ErrAliasTaken          = errors.New("alias already in use for this provider")
	ErrBusy                = errors.New("another addn process is holding the vault; try again in a moment")
)

type InitOptions struct {
	// KeyFile keeps the master key in $ADDN_HOME/master.key (0600) instead of the OS keychain.
	KeyFile bool
}

// InitResult reports the vault; KeySource is the source recorded at init, even when ADDN_MASTER_KEY unlocked it.
type InitResult struct {
	Home          string    `json:"home"`
	KeySource     KeySource `json:"key_source"`
	SchemaVersion int       `json:"schema_version"`
	Created       bool      `json:"created"`
}

// Init creates the vault, or verifies an existing one; it never replaces a key.
func Init(ctx context.Context, env Env, opts InitOptions) (InitResult, error) {
	env = env.withDefaults()
	if opts.KeyFile && env.GOOS == "windows" {
		return InitResult{}, ErrKeyFileUnsupported
	}
	home, err := openHome(env, true)
	if err != nil {
		return InitResult{}, err
	}
	defer home.Close()
	if err := prepareDBFiles(home); err != nil {
		return InitResult{}, err
	}
	db, err := openDB(ctx, home.path)
	if err != nil {
		return InitResult{}, err
	}
	defer db.Close()

	result := InitResult{Home: home.path, SchemaVersion: SchemaVersion}
	tx, err := beginWriterTx(ctx, db)
	if err != nil {
		return InitResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	ctx = context.WithoutCancel(ctx) // once it holds the lock, Init finishes so a published key always gets its record
	if err := home.removeKeyTemps(); err != nil {
		return InitResult{}, err
	}

	rec, found, err := loadInitRecords(ctx, tx)
	if err != nil {
		return InitResult{}, err
	}
	if found {
		if _, err := unlock(env, home, rec); err != nil {
			return InitResult{}, err
		}
		result.KeySource = rec.source
		return result, nil
	}

	if rec.vaultID, err = newVaultID(); err != nil {
		return InitResult{}, err
	}
	key, source, err := provisionKey(env, home, rec.vaultID, opts.KeyFile)
	if err != nil {
		return InitResult{}, err
	}
	canary, err := newCanary(key, rec.vaultID)
	if err != nil {
		return InitResult{}, err
	}
	for k, v := range map[string]string{metaVaultID: rec.vaultID, metaKeySource: string(source), metaKeyCanary: canary} {
		if err := setMeta(ctx, tx, k, v); err != nil {
			return InitResult{}, err
		}
	}
	if err := (ops{q: tx, now: env.Now}).Audit(ctx, InitEvent{KeySource: source}); err != nil {
		return InitResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return InitResult{}, err
	}
	result.KeySource, result.Created = source, true
	return result, nil
}

var sideFileSuffixes = []string{"-wal", "-shm", "-journal"}

// prepareDBFiles checks the home's existing vault files, then creates the database at 0600 if absent.
func prepareDBFiles(home *homeDir) error {
	if err := checkSideFiles(home); err != nil {
		return err
	}
	f, err := home.createPrivateFile(dbFile)
	if err == nil {
		return f.Close()
	}
	if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create vault database: %w", err)
	}
	return home.checkPrivateFile(dbFile)
}

// checkDBFiles requires the database to exist and it and its side files to be private.
func checkDBFiles(home *homeDir) error {
	if err := home.checkPrivateFile(dbFile); err != nil {
		return err
	}
	return checkSideFiles(home)
}

// checkSideFiles requires every existing SQLite side file to be private; SQLite would reuse them as found.
func checkSideFiles(home *homeDir) error {
	for _, suffix := range sideFileSuffixes {
		if err := home.checkPrivateFile(dbFile + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Store is an open vault. All methods take and return plaintext; secrets are sealed at this boundary.
type Store struct {
	ops
	db *sql.DB
}

func notInitializedAt(home string) error {
	return fmt.Errorf("%w (looked in %s)", ErrNotInitialized, home)
}

// Open unlocks an initialised vault, applying any pending migrations.
func Open(ctx context.Context, env Env) (*Store, error) {
	env = env.withDefaults()
	home, err := openHome(env, false)
	if err != nil {
		return nil, err
	}
	defer home.Close()
	if err := checkDBFiles(home); errors.Is(err, fs.ErrNotExist) {
		return nil, notInitializedAt(home.path)
	} else if err != nil {
		return nil, err
	}
	db, err := openDB(ctx, home.path)
	if err != nil {
		return nil, err
	}
	err = withWriterLock(ctx, db, home.removeKeyTemps)
	var rec initRecords
	var found bool
	if err == nil {
		rec, found, err = loadInitRecordsSnapshot(ctx, db)
	}
	if err == nil && !found {
		err = notInitializedAt(home.path)
	}
	var key crypto.Key
	if err == nil {
		key, err = unlock(env, home, rec)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{ops: ops{q: db, key: key, vaultID: rec.vaultID, now: env.Now}, db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// initRecords are the meta rows init writes together in one transaction.
type initRecords struct {
	vaultID string
	source  KeySource
	canary  string
}

// loadInitRecordsSnapshot reads the init records and the empty-vault check in one read transaction.
func loadInitRecordsSnapshot(ctx context.Context, db *sql.DB) (initRecords, bool, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return initRecords{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	return loadInitRecords(ctx, tx)
}

// afterFirstInitRecord lets tests commit a concurrent init between the init-record reads.
var afterFirstInitRecord = func() {}

// loadInitRecords reports found=false only for a vault with no init records and no data.
func loadInitRecords(ctx context.Context, q querier) (initRecords, bool, error) {
	values := map[string]string{}
	for i, k := range []string{metaVaultID, metaKeySource, metaKeyCanary} {
		if i == 1 {
			afterFirstInitRecord()
		}
		v, err := getMeta(ctx, q, k)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return initRecords{}, false, err
		}
		values[k] = v
	}
	rec := initRecords{vaultID: values[metaVaultID], source: KeySource(values[metaKeySource]), canary: values[metaKeyCanary]}
	switch len(values) {
	case 3:
		if !rec.source.valid() || rec.vaultID == "" {
			return initRecords{}, false, fmt.Errorf("%w: unknown key source or vault id", ErrCorrupt)
		}
		return rec, true, nil
	case 0:
		empty, err := holdsNoVaultData(ctx, q)
		if err != nil || empty {
			return initRecords{}, false, err
		}
	}
	return initRecords{}, false, fmt.Errorf("%w: it is only partly initialised; restore it from a backup, or move it aside and run `addn init`", ErrCorrupt)
}

func holdsNoVaultData(ctx context.Context, q querier) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM oauth_clients) + (SELECT count(*) FROM accounts) +
		(SELECT count(*) FROM credentials) + (SELECT count(*) FROM audit_log) +
		(SELECT count(*) FROM meta WHERE key != ?)`, metaSchemaVersion).Scan(&n)
	return n == 0, err
}

// unlock resolves the master key and proves it against the stored canary.
func unlock(env Env, home *homeDir, rec initRecords) (crypto.Key, error) {
	key, err := resolveKey(env, home, rec)
	if err != nil {
		return crypto.Key{}, err
	}
	if err := checkCanary(key, rec); err != nil {
		return crypto.Key{}, err
	}
	return key, nil
}
