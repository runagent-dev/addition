package vault_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/runagent-dev/addition/internal/vault"
)

var errNoKeychain = errors.New("org.freedesktop.secrets was not provided by any .service files")

var testNow = time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)

// fakeEnv is an injected process environment rooted in a temp dir.
type fakeEnv struct {
	vars map[string]string
	home string
	goos string
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	keyring.MockInit()
	home := filepath.Join(privateTempDir(t), "addition-home")
	return &fakeEnv{vars: map[string]string{"ADDITION_HOME": home, "HOME": privateTempDir(t)}, home: home, goos: "linux"}
}

func (f *fakeEnv) set(key, value string) { f.vars[key] = value }

func (f *fakeEnv) unset(key string) { delete(f.vars, key) }

func (f *fakeEnv) env() vault.Env {
	return vault.Env{
		LookupEnv: func(k string) (string, bool) { v, ok := f.vars[k]; return v, ok },
		GOOS:      f.goos,
		Now:       func() time.Time { return testNow },
	}
}

func mustInit(t *testing.T, f *fakeEnv, opts vault.InitOptions) vault.InitResult {
	t.Helper()
	res, err := vault.Init(context.Background(), f.env(), opts)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return res
}

func mustOpen(t *testing.T, f *fakeEnv) *vault.Store {
	t.Helper()
	s, err := vault.Open(context.Background(), f.env())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newStore initialises a vault backed by a key file and opens it.
func newStore(t *testing.T) (*vault.Store, *fakeEnv) {
	t.Helper()
	f := newFakeEnv(t)
	mustInit(t, f, vault.InitOptions{KeyFile: true})
	return mustOpen(t, f), f
}

// vaultID reads the random ID that names the vault's keychain entry.
func vaultID(t *testing.T, home string) string {
	t.Helper()
	var id string
	if err := rawDB(t, home).QueryRow(`SELECT value FROM meta WHERE key = 'vault_id'`).Scan(&id); err != nil {
		t.Fatalf("read vault_id: %v", err)
	}
	return id
}

// keychainKey returns the vault's key from the mock keychain, or "" when it has none.
func keychainKey(t *testing.T, home string) string {
	t.Helper()
	key, err := keyring.Get(vault.KeychainService, vaultID(t, home))
	if errors.Is(err, keyring.ErrNotFound) {
		return ""
	}
	if err != nil {
		t.Fatalf("keychain: %v", err)
	}
	return key
}

func rawDB(t *testing.T, home string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "addition.db"))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// dumpAllText concatenates every value in every user table.
func dumpAllText(t *testing.T, home string) string {
	t.Helper()
	db := rawDB(t, home)
	tables, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var names []string
	for tables.Next() {
		var n string
		_ = tables.Scan(&n)
		names = append(names, n)
	}
	_ = tables.Close()

	var all strings.Builder
	for _, name := range names {
		rows, err := db.Query(`SELECT * FROM "` + name + `"`)
		if err != nil {
			t.Fatalf("dump %s: %v", name, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			_ = rows.Scan(ptrs...)
			for _, v := range vals {
				switch x := v.(type) {
				case string:
					all.WriteString(x + "\n")
				case []byte:
					all.WriteString(string(x) + "\n")
				}
			}
		}
		_ = rows.Close()
	}
	return all.String()
}
