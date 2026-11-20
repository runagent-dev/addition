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
	stored, err := keyring.Get(vault.KeychainService, f.home)
	if err != nil {
		t.Fatalf("keychain has no key: %v", err)
	}
	if _, err := crypto.ParseKey(stored); err != nil {
		t.Errorf("keychain value is not a key: %v", err)
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
	if _, err := keyring.Get(vault.KeychainService, f.home); !errors.Is(err, keyring.ErrNotFound) {
		t.Errorf("keychain was written with --key-file (err = %v)", err)
	}
}

func TestInitWithEnvKey(t *testing.T) {
	f := newFakeEnv(t)
	f.set("ADDN_MASTER_KEY", strings.Repeat("d", 64))

	res := mustInit(t, f, vault.InitOptions{KeyFile: true})

	if res.KeySource != vault.KeySourceEnv || !res.Created {
		t.Errorf("Init = %+v, want key_source=env created=true", res)
	}
	if _, err := os.Stat(filepath.Join(f.home, "master.key")); !errors.Is(err, os.ErrNotExist) {
		t.Error("master.key written although ADDN_MASTER_KEY is set")
	}
	if _, err := keyring.Get(vault.KeychainService, f.home); !errors.Is(err, keyring.ErrNotFound) {
		t.Error("keychain written although ADDN_MASTER_KEY is set")
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
	for _, hint := range []string{"--key-file", "ADDN_MASTER_KEY"} {
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
	f.set("ADDN_MASTER_KEY", strings.Repeat("a", 64)+"\n")

	_, err := vault.Init(context.Background(), f.env(), vault.InitOptions{})

	if !errors.Is(err, crypto.ErrInvalidKey) {
		t.Fatalf("err = %v, want ErrInvalidKey", err)
	}
}

func TestInitRerunWithWrongEnvKeyFails(t *testing.T) {
	f := newFakeEnv(t)
	f.set("ADDN_MASTER_KEY", strings.Repeat("d", 64))
	mustInit(t, f, vault.InitOptions{})

	f.set("ADDN_MASTER_KEY", strings.Repeat("e", 64))
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
	if v, err := keyring.Get(vault.KeychainService, f.home); err == nil {
		return v
	}
	content, err := os.ReadFile(filepath.Join(f.home, "master.key"))
	if err != nil {
		t.Fatalf("no key in keychain or key file: %v", err)
	}
	return string(content)
}
