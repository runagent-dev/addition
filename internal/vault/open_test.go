package vault_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/runagent-dev/addition/internal/crypto"
	"github.com/runagent-dev/addition/internal/vault"
)

func openErr(t *testing.T, f *fakeEnv) error {
	t.Helper()
	s, err := vault.Open(context.Background(), f.env())
	if err == nil {
		_ = s.Close()
	}
	return err
}

func TestOpenBeforeInit(t *testing.T) {
	f := newFakeEnv(t)

	err := openErr(t, f)

	if !errors.Is(err, vault.ErrNotInitialized) {
		t.Fatalf("err = %v, want ErrNotInitialized", err)
	}
	if !strings.Contains(err.Error(), "addition init") {
		t.Errorf("error %q does not say what to run", err)
	}
	if _, err := os.Stat(f.home); !errors.Is(err, os.ErrNotExist) {
		t.Error("Open created the data dir")
	}
}

func TestOpenResolvesEachKeySource(t *testing.T) {
	t.Run("keychain", func(t *testing.T) {
		f := newFakeEnv(t)
		mustInit(t, f, vault.InitOptions{})
		mustOpen(t, f)
	})
	t.Run("key file, keychain unavailable", func(t *testing.T) {
		f := newFakeEnv(t)
		mustInit(t, f, vault.InitOptions{KeyFile: true})
		keyring.MockInitWithError(errNoKeychain)
		mustOpen(t, f)
	})
	t.Run("env", func(t *testing.T) {
		f := newFakeEnv(t)
		f.set("ADDITION_MASTER_KEY", strings.Repeat("d", 64))
		mustInit(t, f, vault.InitOptions{})
		mustOpen(t, f)
	})
	t.Run("env overrides the keychain", func(t *testing.T) {
		f := newFakeEnv(t)
		mustInit(t, f, vault.InitOptions{})
		stored := keychainKey(t, f.home)
		keyring.MockInit()
		f.set("ADDITION_MASTER_KEY", stored)
		mustOpen(t, f)
	})
}

func TestOpenWithWrongKeyFailsClearly(t *testing.T) {
	f := newFakeEnv(t)
	mustInit(t, f, vault.InitOptions{})
	f.set("ADDITION_MASTER_KEY", strings.Repeat("e", 64))

	err := openErr(t, f)

	if !errors.Is(err, vault.ErrWrongMasterKey) {
		t.Fatalf("err = %v, want ErrWrongMasterKey", err)
	}
}

func TestOpenRejectsAnInvalidEnvKey(t *testing.T) {
	f := newFakeEnv(t)
	mustInit(t, f, vault.InitOptions{})
	f.set("ADDITION_MASTER_KEY", strings.Repeat("a", 64)+"\n")

	if err := openErr(t, f); !errors.Is(err, crypto.ErrInvalidKey) {
		t.Fatalf("err = %v, want ErrInvalidKey", err)
	}
}

func TestOpenNeverUsesAnUnrecordedKeyFile(t *testing.T) {
	f := newFakeEnv(t)
	mustInit(t, f, vault.InitOptions{})
	stored := keychainKey(t, f.home)
	if err := os.WriteFile(filepath.Join(f.home, "master.key"), []byte(stored), 0o600); err != nil {
		t.Fatal(err)
	}
	keyring.MockInitWithError(errNoKeychain)

	err := openErr(t, f)

	if !errors.Is(err, vault.ErrNoMasterKey) {
		t.Fatalf("err = %v, want ErrNoMasterKey", err)
	}
}

func TestOpenFailsLoudlyWhenNoKeySourceIsAvailable(t *testing.T) {
	cases := map[string]func(t *testing.T, f *fakeEnv){
		"keychain vault, keychain gone": func(t *testing.T, f *fakeEnv) {
			mustInit(t, f, vault.InitOptions{})
			keyring.MockInit()
		},
		"keychain vault, keychain unavailable": func(t *testing.T, f *fakeEnv) {
			mustInit(t, f, vault.InitOptions{})
			keyring.MockInitWithError(errNoKeychain)
		},
		"env vault, env unset": func(t *testing.T, f *fakeEnv) {
			f.set("ADDITION_MASTER_KEY", strings.Repeat("d", 64))
			mustInit(t, f, vault.InitOptions{})
			f.unset("ADDITION_MASTER_KEY")
		},
		"key file vault, file deleted": func(t *testing.T, f *fakeEnv) {
			mustInit(t, f, vault.InitOptions{KeyFile: true})
			_ = os.Remove(filepath.Join(f.home, "master.key"))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeEnv(t)
			setup(t, f)

			err := openErr(t, f)

			if !errors.Is(err, vault.ErrNoMasterKey) {
				t.Fatalf("err = %v, want ErrNoMasterKey", err)
			}
			if !strings.Contains(err.Error(), "ADDITION_MASTER_KEY") {
				t.Errorf("error %q does not say what to do", err)
			}
		})
	}
}

func TestOpenRejectsAKeyFileOthersCanRead(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		t.Run(mode.String(), func(t *testing.T) {
			f := newFakeEnv(t)
			mustInit(t, f, vault.InitOptions{KeyFile: true})
			if err := os.Chmod(filepath.Join(f.home, "master.key"), mode); err != nil {
				t.Fatal(err)
			}

			if err := openErr(t, f); !errors.Is(err, vault.ErrInsecureKeyFile) {
				t.Fatalf("err = %v, want ErrInsecureKeyFile", err)
			}
		})
	}
}

func TestMigrationsCreateTheSchema(t *testing.T) {
	_, f := newStore(t)
	db := rawDB(t, f.home)

	for _, table := range []string{"meta", "oauth_clients", "accounts", "credentials", "audit_log", "pending_auth"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing (n=%d err=%v)", table, n, err)
		}
	}
	var version string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&version); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if version != "4" || vault.SchemaVersion != 4 {
		t.Errorf("schema_version = %s (binary %d), want 4", version, vault.SchemaVersion)
	}
	var mode string
	_ = db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

func TestRefusesADatabaseFromANewerBinary(t *testing.T) {
	s, f := newStore(t)
	_ = s.Close()
	if _, err := rawDB(t, f.home).Exec(`UPDATE meta SET value='999' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}

	if err := openErr(t, f); !errors.Is(err, vault.ErrSchemaTooNew) {
		t.Errorf("Open err = %v, want ErrSchemaTooNew", err)
	}
	if _, err := vault.Init(context.Background(), f.env(), vault.InitOptions{}); !errors.Is(err, vault.ErrSchemaTooNew) {
		t.Errorf("Init err = %v, want ErrSchemaTooNew", err)
	}
}

func TestOpenTreatsASetButEmptyEnvKeyAsAnError(t *testing.T) {
	f := newFakeEnv(t)
	mustInit(t, f, vault.InitOptions{KeyFile: true})
	f.set("ADDITION_MASTER_KEY", "")

	if err := openErr(t, f); !errors.Is(err, crypto.ErrInvalidKey) {
		t.Fatalf("err = %v, want ErrInvalidKey for a set-but-empty ADDITION_MASTER_KEY", err)
	}
}

func TestOpenUsesOnlyTheRecordedSource(t *testing.T) {
	f := newFakeEnv(t)
	mustInit(t, f, vault.InitOptions{})
	if err := os.WriteFile(filepath.Join(f.home, "master.key"), []byte(keychainKey(t, f.home)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = rawDB(t, f.home).Exec(`UPDATE meta SET value = 'file' WHERE key = 'key_source'`)
	_ = keyring.Set(vault.KeychainService, vaultID(t, f.home), strings.Repeat("e", 64))

	mustOpen(t, f)
}

func TestAKeychainVaultSurvivesRelocation(t *testing.T) {
	f := newFakeEnv(t)
	mustInit(t, f, vault.InitOptions{})
	moved := filepath.Join(privateTempDir(t), "moved-home")
	if err := os.Rename(f.home, moved); err != nil {
		t.Fatal(err)
	}
	f.set("ADDITION_HOME", moved)

	mustOpen(t, f)
}

func TestHomePathPunctuationIsNotParsedAsDSN(t *testing.T) {
	f := newFakeEnv(t)
	parent := privateTempDir(t)
	f.home = filepath.Join(parent, "we?ird#na%me")
	f.set("ADDITION_HOME", f.home)

	mustInit(t, f, vault.InitOptions{KeyFile: true})
	mustOpen(t, f)

	if _, err := os.Stat(filepath.Join(f.home, "addition.db")); err != nil {
		t.Errorf("addition.db is not inside the punctuated home: %v", err)
	}
	entries, _ := os.ReadDir(parent)
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("parent dir holds %v, want only the home", names)
	}
}

func TestRejectsACorruptSchemaVersion(t *testing.T) {
	for _, version := range []string{"-1", "0", "one"} {
		t.Run(version, func(t *testing.T) {
			s, f := newStore(t)
			_ = s.Close()
			if _, err := rawDB(t, f.home).Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`, version); err != nil {
				t.Fatal(err)
			}

			err := openErr(t, f)

			if !errors.Is(err, vault.ErrCorrupt) {
				t.Fatalf("err = %v, want ErrCorrupt", err)
			}
			if strings.Contains(err.Error(), version) && version != "0" {
				t.Errorf("error %q echoes the raw value", err)
			}
		})
	}
}
