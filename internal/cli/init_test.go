package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/runagent-dev/addition/internal/vault"
)

type initOutput struct {
	Home          string `json:"home"`
	KeySource     string `json:"key_source"`
	SchemaVersion int    `json:"schema_version"`
	Created       bool   `json:"created"`
}

// isolatedHome points every data-dir variable at temp dirs and mocks the keychain.
func isolatedHome(t *testing.T) string {
	t.Helper()
	keyring.MockInit()
	home := filepath.Join(t.TempDir(), "addn")
	t.Setenv("ADDN_HOME", home)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("ADDN_MASTER_KEY", "")
	return home
}

func decodeInit(t *testing.T, r result) initOutput {
	t.Helper()
	if r.code != ExitOK {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", r.code, r.stderr)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want empty", r.stderr)
	}
	var out initOutput
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("stdout is not JSON: %v\nstdout: %q", err, r.stdout)
	}
	return out
}

func assertNoLeak(t *testing.T, r result, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if s == "" {
			t.Fatal("assertNoLeak called with an empty secret")
		}
		if strings.Contains(r.stdout, s) || strings.Contains(r.stderr, s) {
			t.Errorf("key material leaked into output\nstdout: %q\nstderr: %q", r.stdout, r.stderr)
		}
	}
}

func TestInitWithKeyFileTwice(t *testing.T) {
	home := isolatedHome(t)

	first := run("init", "--key-file")
	got := decodeInit(t, first)
	want := initOutput{Home: home, KeySource: "file", SchemaVersion: vault.SchemaVersion, Created: true}
	if got != want {
		t.Errorf("first init = %+v, want %+v", got, want)
	}

	key, err := os.ReadFile(filepath.Join(home, "master.key"))
	if err != nil {
		t.Fatalf("read master.key: %v", err)
	}

	second := run("init", "--key-file")
	want.Created = false
	if got := decodeInit(t, second); got != want {
		t.Errorf("second init = %+v, want %+v", got, want)
	}
	again, _ := os.ReadFile(filepath.Join(home, "master.key"))
	if string(again) != string(key) {
		t.Error("re-running init changed the key file")
	}
	assertNoLeak(t, first, string(key))
	assertNoLeak(t, second, string(key))
}

func TestInitWithKeychain(t *testing.T) {
	home := isolatedHome(t)

	r := run("init")

	got := decodeInit(t, r)
	if got.KeySource != "keychain" || !got.Created || got.Home != home {
		t.Errorf("init = %+v, want key_source=keychain created=true home=%s", got, home)
	}
	key, err := keyring.Get(vault.KeychainService, home)
	if err != nil {
		t.Fatalf("keychain: %v", err)
	}
	assertNoLeak(t, r, key)
}

func TestInitWithEnvKey(t *testing.T) {
	isolatedHome(t)
	key := strings.Repeat("d", 64)
	t.Setenv("ADDN_MASTER_KEY", key)

	r := run("init")

	if got := decodeInit(t, r); got.KeySource != "env" || !got.Created {
		t.Errorf("init = %+v, want key_source=env created=true", got)
	}
	assertNoLeak(t, r, key)

	wrong := strings.Repeat("e", 64)
	t.Setenv("ADDN_MASTER_KEY", wrong)
	r = run("init")
	if r.code == ExitOK {
		t.Fatal("init with the wrong ADDN_MASTER_KEY succeeded")
	}
	if env := decodeError(t, r.stderr); env.Error.Code != "wrong-master-key" {
		t.Errorf("error.code = %q, want wrong-master-key", env.Error.Code)
	}
	assertNoLeak(t, r, key, wrong)
}

func TestInitRejectsAMalformedEnvKeyWithoutEchoingIt(t *testing.T) {
	isolatedHome(t)
	bad := strings.Repeat("f", 64) + "\n"
	t.Setenv("ADDN_MASTER_KEY", bad)

	r := run("init")

	if r.code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", r.code, ExitUsage)
	}
	if env := decodeError(t, r.stderr); env.Error.Code != "invalid-master-key" {
		t.Errorf("error.code = %q, want invalid-master-key", env.Error.Code)
	}
	assertNoLeak(t, r, strings.Repeat("f", 64))
}

func TestInitWithoutKeychainSuggestsAlternatives(t *testing.T) {
	home := isolatedHome(t)
	keyring.MockInitWithError(errors.New("no secret service"))

	r := run("init")

	if r.code != ExitUsage {
		t.Fatalf("exit code = %d, want %d (stderr %q)", r.code, ExitUsage, r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q, want empty", r.stdout)
	}
	env := decodeError(t, r.stderr)
	if env.Error.Code != "keychain-unavailable" {
		t.Errorf("error.code = %q, want keychain-unavailable", env.Error.Code)
	}
	for _, hint := range []string{"--key-file", "ADDN_MASTER_KEY"} {
		if !strings.Contains(env.Error.Message, hint) {
			t.Errorf("message %q does not suggest %s", env.Error.Message, hint)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "master.key")); !errors.Is(err, os.ErrNotExist) {
		t.Error("init wrote a key file nobody opted into")
	}
}

func TestInitRejectsArguments(t *testing.T) {
	isolatedHome(t)
	if r := run("init", "extra"); r.code != ExitUsage {
		t.Errorf("exit code = %d, want %d", r.code, ExitUsage)
	}
}

func TestUnexpectedVaultFailureIsInternal(t *testing.T) {
	failure := vaultError(errors.New("disk I/O error"))

	if failure.Exit != ExitInternal || failure.Code != "vault" {
		t.Errorf("got exit %d code %q, want exit %d code vault", failure.Exit, failure.Code, ExitInternal)
	}
}
