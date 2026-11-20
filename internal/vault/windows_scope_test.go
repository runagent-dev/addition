package vault_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runagent-dev/addition/internal/vault"
)

const testMasterKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// windowsEnv injects GOOS=windows with a profile that contains f.home.
func windowsEnv(t *testing.T, f *fakeEnv) {
	t.Helper()
	f.goos = "windows"
	f.set("USERPROFILE", filepath.Dir(f.home))
}

func TestWindowsRefusesInitWithAKeyFile(t *testing.T) {
	f := newFakeEnv(t)
	windowsEnv(t, f)
	f.set("ADDN_MASTER_KEY", testMasterKey)

	_, err := vault.Init(context.Background(), f.env(), vault.InitOptions{KeyFile: true})

	if !errors.Is(err, vault.ErrKeyFileUnsupported) {
		t.Fatalf("err = %v, want ErrKeyFileUnsupported", err)
	}
	for _, hint := range []string{"Credential Manager", "ADDN_MASTER_KEY"} {
		if !strings.Contains(err.Error(), hint) {
			t.Errorf("message %q does not mention %s", err, hint)
		}
	}
	if _, err := os.Stat(f.home); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the data dir was created (stat err = %v)", err)
	}
}

func TestWindowsRefusesAFileBackedOpen(t *testing.T) {
	f := newFakeEnv(t)
	mustInit(t, f, vault.InitOptions{KeyFile: true})
	windowsEnv(t, f)

	if err := openErr(t, f); !errors.Is(err, vault.ErrKeyFileUnsupported) {
		t.Fatalf("err = %v, want ErrKeyFileUnsupported", err)
	}
}

func TestWindowsRequiresAHomeInsideTheProfile(t *testing.T) {
	f := newFakeEnv(t)
	windowsEnv(t, f)
	f.set("USERPROFILE", filepath.Join(privateTempDir(t), "profile"))

	_, initErr := vault.Init(context.Background(), f.env(), vault.InitOptions{})
	for op, err := range map[string]error{"Init": initErr, "Open": openErr(t, f)} {
		if !errors.Is(err, vault.ErrInsecureHome) || !strings.Contains(err.Error(), "USERPROFILE") {
			t.Errorf("%s err = %v, want ErrInsecureHome naming USERPROFILE", op, err)
		}
	}
	if _, err := os.Stat(f.home); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the data dir was created outside the profile (stat err = %v)", err)
	}
}

func TestWindowsAcceptsAHomeInsideTheProfile(t *testing.T) {
	f := newFakeEnv(t)
	windowsEnv(t, f)

	if res := mustInit(t, f, vault.InitOptions{}); res.KeySource != vault.KeySourceKeychain {
		t.Errorf("key source = %q, want keychain", res.KeySource)
	}
	mustOpen(t, f)
}

func TestWindowsRejectsAProfileLinkThatLeavesIt(t *testing.T) {
	f := newFakeEnv(t)
	windowsEnv(t, f)
	outside := filepath.Join(privateTempDir(t), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, f.home); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	_, err := vault.Init(context.Background(), f.env(), vault.InitOptions{})
	if !errors.Is(err, vault.ErrInsecureHome) {
		t.Fatalf("err = %v, want ErrInsecureHome", err)
	}
}
