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

func TestInitWithKeychain(t *testing.T) {
	f := newFakeEnv(t)

	res := mustInit(t, f, vault.InitOptions{})

	want := vault.InitResult{Home: f.home, KeySource: vault.KeySourceKeychain, SchemaVersion: vault.SchemaVersion, Created: true}
	if res != want {
		t.Errorf("Init = %+v, want %+v", res, want)
	}
	if _, err := crypto.ParseKey(keychainKey(t, f.home)); err != nil {
		t.Errorf("keychain value under the vault ID is not a key: %v", err)
	}
	if _, err := keyring.Get(vault.KeychainService, f.home); !errors.Is(err, keyring.ErrNotFound) {
		t.Errorf("keychain entry is indexed by the home path (err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(f.home, "master.key")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("master.key exists without --key-file (stat err = %v)", err)
	}
	assertMode(t, f.home, 0o700)
	if _, err := os.Stat(filepath.Join(f.home, "addition.db")); err != nil {
		t.Errorf("addition.db missing: %v", err)
	}
}

func TestInitWithKeyFile(t *testing.T) {
	f := newFakeEnv(t)

	res := mustInit(t, f, vault.InitOptions{KeyFile: true})

	if res.KeySource != vault.KeySourceFile || !res.Created {
		t.Errorf("Init = %+v, want key_source=file created=true", res)
	}
	path := filepath.Join(f.home, "master.key")
	assertMode(t, path, 0o600)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read master.key: %v", err)
	}
	if _, err := crypto.ParseKey(string(content)); err != nil {
		t.Errorf("master.key is not exactly a key: %v", err)
	}
	if keychainKey(t, f.home) != "" {
		t.Error("keychain was written with --key-file")
	}
}

func TestInitWithEnvKey(t *testing.T) {
	f := newFakeEnv(t)
	f.set("ADDITION_MASTER_KEY", strings.Repeat("d", 64))

	res := mustInit(t, f, vault.InitOptions{KeyFile: true})

	if res.KeySource != vault.KeySourceEnv || !res.Created {
		t.Errorf("Init = %+v, want key_source=env created=true", res)
	}
	if _, err := os.Stat(filepath.Join(f.home, "master.key")); !errors.Is(err, os.ErrNotExist) {
		t.Error("master.key written although ADDITION_MASTER_KEY is set")
	}
	if keychainKey(t, f.home) != "" {
		t.Error("keychain written although ADDITION_MASTER_KEY is set")
	}
}

func TestInitIsIdempotent(t *testing.T) {
	cases := map[string]vault.InitOptions{
		"keychain": {},
		"key file": {KeyFile: true},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeEnv(t)
			first := mustInit(t, f, opts)
			keyBefore := currentKeyMaterial(t, f)

			for _, rerun := range []vault.InitOptions{opts, {KeyFile: !opts.KeyFile}} {
				again := mustInit(t, f, rerun)
				if again.Created {
					t.Errorf("re-run reported created=true")
				}
				if again.KeySource != first.KeySource {
					t.Errorf("re-run key_source = %q, want %q", again.KeySource, first.KeySource)
				}
				if got := currentKeyMaterial(t, f); got != keyBefore {
					t.Error("re-run changed the stored key")
				}
			}
		})
	}
}

func TestInitFailsLoudlyWhenKeychainUnavailable(t *testing.T) {
	f := newFakeEnv(t)
	keyring.MockInitWithError(errNoKeychain)

	_, err := vault.Init(context.Background(), f.env(), vault.InitOptions{})

	if !errors.Is(err, vault.ErrKeychainUnavailable) {
		t.Fatalf("err = %v, want ErrKeychainUnavailable", err)
	}
	for _, hint := range []string{"--key-file", "ADDITION_MASTER_KEY"} {
		if !strings.Contains(err.Error(), hint) {
			t.Errorf("error %q does not suggest %s", err, hint)
		}
	}
	if _, err := os.Stat(filepath.Join(f.home, "master.key")); !errors.Is(err, os.ErrNotExist) {
		t.Error("fell back to a key file nobody opted into")
	}

	res := mustInit(t, f, vault.InitOptions{KeyFile: true})
	if !res.Created || res.KeySource != vault.KeySourceFile {
		t.Errorf("Init --key-file after failure = %+v, want a fresh key-file vault", res)
	}
}

func TestInitRefusesAnInvalidEnvKey(t *testing.T) {
	f := newFakeEnv(t)
	f.set("ADDITION_MASTER_KEY", strings.Repeat("a", 64)+"\n")

	_, err := vault.Init(context.Background(), f.env(), vault.InitOptions{})

	if !errors.Is(err, crypto.ErrInvalidKey) {
		t.Fatalf("err = %v, want ErrInvalidKey", err)
	}
}

func TestInitRerunWithWrongEnvKeyFails(t *testing.T) {
	f := newFakeEnv(t)
	f.set("ADDITION_MASTER_KEY", strings.Repeat("d", 64))
	mustInit(t, f, vault.InitOptions{})

	f.set("ADDITION_MASTER_KEY", strings.Repeat("e", 64))
	_, err := vault.Init(context.Background(), f.env(), vault.InitOptions{})

	if !errors.Is(err, vault.ErrWrongMasterKey) {
		t.Fatalf("err = %v, want ErrWrongMasterKey", err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}

func currentKeyMaterial(t *testing.T, f *fakeEnv) string {
	t.Helper()
	if v := keychainKey(t, f.home); v != "" {
		return v
	}
	content, err := os.ReadFile(filepath.Join(f.home, "master.key"))
	if err != nil {
		t.Fatalf("no key in keychain or key file: %v", err)
	}
	return string(content)
}

func TestInitTreatsASetButEmptyEnvKeyAsAnError(t *testing.T) {
	f := newFakeEnv(t)
	f.set("ADDITION_MASTER_KEY", "")

	_, err := vault.Init(context.Background(), f.env(), vault.InitOptions{KeyFile: true})

	if !errors.Is(err, crypto.ErrInvalidKey) || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want ErrInvalidKey saying the variable is empty", err)
	}
	if _, err := os.Stat(filepath.Join(f.home, "master.key")); !errors.Is(err, os.ErrNotExist) {
		t.Error("an empty ADDITION_MASTER_KEY fell through to a key file")
	}
}

func TestKeyFileVaultIgnoresAStaleKeychainEntry(t *testing.T) {
	f := newFakeEnv(t)
	stale := strings.Repeat("e", 64)
	_ = keyring.Set(vault.KeychainService, f.home, stale)
	first := mustInit(t, f, vault.InitOptions{KeyFile: true})
	_ = keyring.Set(vault.KeychainService, vaultID(t, f.home), stale)

	again := mustInit(t, f, vault.InitOptions{KeyFile: true})

	if first.KeySource != vault.KeySourceFile || again.KeySource != vault.KeySourceFile || again.Created {
		t.Errorf("first = %+v, rerun = %+v; want a file vault both times", first, again)
	}
	mustOpen(t, f)
}

func TestInitWithoutKeyFileAlwaysMeansKeychain(t *testing.T) {
	f := newFakeEnv(t)
	if err := os.MkdirAll(f.home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "master.key"), []byte(strings.Repeat("c", 64)), 0o600); err != nil {
		t.Fatal(err)
	}

	res := mustInit(t, f, vault.InitOptions{})

	if res.KeySource != vault.KeySourceKeychain {
		t.Errorf("key_source = %q, want keychain even though master.key exists", res.KeySource)
	}
}

func TestInitReportsTheRecordedSourceWhenEnvUnlocks(t *testing.T) {
	f := newFakeEnv(t)
	mustInit(t, f, vault.InitOptions{})
	f.set("ADDITION_MASTER_KEY", keychainKey(t, f.home))
	keyring.MockInitWithError(errNoKeychain)

	res := mustInit(t, f, vault.InitOptions{})

	if res.KeySource != vault.KeySourceKeychain || res.Created {
		t.Errorf("Init = %+v, want the recorded keychain source and created=false", res)
	}
}

func TestInitRefusesAPartiallyInitializedVault(t *testing.T) {
	cases := map[string]string{
		"no key_source":           `DELETE FROM meta WHERE key = 'key_source'`,
		"no key_canary":           `DELETE FROM meta WHERE key = 'key_canary'`,
		"no vault_id":             `DELETE FROM meta WHERE key = 'vault_id'`,
		"records gone, data left": `DELETE FROM meta WHERE key IN ('key_source', 'key_canary', 'vault_id')`,
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeEnv(t)
			f.set("ADDITION_MASTER_KEY", strings.Repeat("d", 64))
			mustInit(t, f, vault.InitOptions{})
			db := rawDB(t, f.home)
			if _, err := db.Exec(damage); err != nil {
				t.Fatal(err)
			}
			var canaryBefore string
			_ = db.QueryRow(`SELECT COALESCE((SELECT value FROM meta WHERE key = 'key_canary'), '')`).Scan(&canaryBefore)
			f.set("ADDITION_MASTER_KEY", strings.Repeat("e", 64))

			_, initErr := vault.Init(context.Background(), f.env(), vault.InitOptions{})
			openErr := openErr(t, f)

			for op, err := range map[string]error{"Init": initErr, "Open": openErr} {
				if !errors.Is(err, vault.ErrCorrupt) {
					t.Errorf("%s err = %v, want ErrCorrupt", op, err)
				}
			}
			var canaryAfter string
			_ = db.QueryRow(`SELECT COALESCE((SELECT value FROM meta WHERE key = 'key_canary'), '')`).Scan(&canaryAfter)
			if canaryAfter != canaryBefore {
				t.Error("Init rewrote the canary of a partially initialized vault")
			}
		})
	}
}

func TestInitReusesAKeyFileLeftByAnInterruptedInit(t *testing.T) {
	f := newFakeEnv(t)
	if err := os.MkdirAll(f.home, 0o700); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("c", 64)
	if err := os.WriteFile(filepath.Join(f.home, "master.key"), []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}

	mustInit(t, f, vault.InitOptions{KeyFile: true})

	f.set("ADDITION_MASTER_KEY", key)
	mustOpen(t, f)
}

func TestInitWritesTheKeyFileWithoutLeftovers(t *testing.T) {
	f := newFakeEnv(t)
	mustInit(t, f, vault.InitOptions{KeyFile: true})

	entries, err := os.ReadDir(f.home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if name := e.Name(); name != "master.key" && !strings.HasPrefix(name, "addition.db") {
			t.Errorf("unexpected file %q left in the data dir", name)
		}
	}
}

func TestATruncatedKeyFileFailsClearly(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "truncated": strings.Repeat("c", 40)} {
		t.Run(name, func(t *testing.T) {
			f := newFakeEnv(t)
			mustInit(t, f, vault.InitOptions{KeyFile: true})
			if err := os.WriteFile(filepath.Join(f.home, "master.key"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}

			err := openErr(t, f)

			if !errors.Is(err, crypto.ErrInvalidKey) || !strings.Contains(err.Error(), "master.key") {
				t.Errorf("err = %v, want ErrInvalidKey naming master.key", err)
			}
		})
	}
}
