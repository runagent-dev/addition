//go:build windows

package vault_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/runagent-dev/addition/internal/vault"
)

// realWindowsEnv uses the real profile but an env-provided key, so Credential Manager is untouched.
func realWindowsEnv(home string) vault.Env {
	env := vault.OSEnv()
	lookup := env.LookupEnv
	env.LookupEnv = func(k string) (string, bool) {
		switch k {
		case "ADDITION_HOME":
			return home, true
		case "ADDITION_MASTER_KEY":
			return testMasterKey, true
		}
		return lookup(k)
	}
	return env
}

func TestWindowsHostAcceptsATempHomeInsideTheProfile(t *testing.T) {
	home := filepath.Join(t.TempDir(), "addition")
	if _, err := vault.Init(context.Background(), realWindowsEnv(home), vault.InitOptions{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
}

func TestWindowsHostRejectsAHomeOutsideTheProfile(t *testing.T) {
	home := filepath.Join(os.Getenv("SystemDrive")+`\`, "addition-outside-profile-test")
	_, err := vault.Init(context.Background(), realWindowsEnv(home), vault.InitOptions{})
	if !errors.Is(err, vault.ErrInsecureHome) {
		t.Fatalf("err = %v, want ErrInsecureHome", err)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		_ = os.RemoveAll(home)
		t.Errorf("the data dir was created outside the profile")
	}
}

func TestWindowsHostRefusesAKeyFile(t *testing.T) {
	home := filepath.Join(t.TempDir(), "addition")
	_, err := vault.Init(context.Background(), realWindowsEnv(home), vault.InitOptions{KeyFile: true})
	if !errors.Is(err, vault.ErrKeyFileUnsupported) {
		t.Fatalf("err = %v, want ErrKeyFileUnsupported", err)
	}
}
