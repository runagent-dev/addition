package vault

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

const dbFile = "addition.db"

// dsn is a file: URI for the existing database; mode=rw stops SQLite creating it with its own permissions.
// Every transaction is BEGIN IMMEDIATE, so a writer takes the lock before it reads.
func dsn(home string) string {
	path := filepath.ToSlash(filepath.Join(home, dbFile))
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	params := url.Values{
		"mode":          {"rw"},
		"_busy_timeout": {"10000"},
		"_journal_mode": {"WAL"},
		"_foreign_keys": {"1"},
		"_synchronous":  {"NORMAL"},
		"_txlock":       {"immediate"},
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: params.Encode()}).String()
}

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version int
	sql     string
}

var migrations = mustLoadMigrations()

// SchemaVersion is the newest schema this binary understands.
var SchemaVersion = migrations[len(migrations)-1].version

func mustLoadMigrations() []migration {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		panic(err)
	}
	var out []migration
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		version, err := strconv.Atoi(prefix)
		if !ok || err != nil {
			panic("migration name must be NNN_name.sql: " + e.Name())
		}
		body, err := migrationFiles.ReadFile("migrations/" + e.Name())
		if err != nil {
			panic(err)
		}
		out = append(out, migration{version: version, sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			panic(fmt.Sprintf("migrations must be numbered 1..n without gaps; found %d at position %d", m.version, i+1))
		}
	}
	return out
}

func openDB(ctx context.Context, home string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(home))
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// beforeWriterLock lets tests act just before a process waits for the SQLite writer lock.
var beforeWriterLock = func() {}

// beginWriterTx takes the SQLite writer lock (BEGIN IMMEDIATE) that every write and key publication holds.
// The lock ignores caller cancellation; database/sql would otherwise roll back and release it mid-work.
func beginWriterTx(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	beforeWriterLock()
	return db.BeginTx(context.WithoutCancel(ctx), nil)
}

// withWriterLock runs fn while holding the writer lock, and commits nothing.
func withWriterLock(ctx context.Context, db *sql.DB, fn func() error) error {
	tx, err := beginWriterTx(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn()
}

// migrate applies pending migrations forward-only, inside one immediate transaction.
func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := beginWriterTx(ctx, db)
	if err != nil {
		return fmt.Errorf("open vault database: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("bootstrap meta: %w", err)
	}
	current, err := schemaVersion(ctx, tx)
	if err != nil {
		return err
	}
	if current > SchemaVersion {
		return fmt.Errorf("%w: database is at version %d, this addn supports up to %d; upgrade addn", ErrSchemaTooNew, current, SchemaVersion)
	}
	if current == SchemaVersion {
		return nil
	}
	for _, m := range migrations[current:] {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("apply migration %03d: %w", m.version, err)
		}
	}
	if err := setMeta(ctx, tx, metaSchemaVersion, strconv.Itoa(SchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func schemaVersion(ctx context.Context, q querier) (int, error) {
	raw, err := getMeta(ctx, q, metaSchemaVersion)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 1 {
		return 0, fmt.Errorf("%w: schema_version is not a valid version", ErrCorrupt)
	}
	return v, nil
}

const (
	metaSchemaVersion = "schema_version"
	metaVaultID       = "vault_id"
	metaKeySource     = "key_source"
	metaKeyCanary     = "key_canary"
)

func getMeta(ctx context.Context, q querier, key string) (string, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func setMeta(ctx context.Context, q querier, key, value string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
