//go:build unix

package vault_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/vault"
)

func TestRejectsAnExistingHomeOthersCanAccess(t *testing.T) {
	for _, mode := range []os.FileMode{0o777, 0o750, 0o705} {
		t.Run(mode.String(), func(t *testing.T) {
			f := newFakeEnv(t)
			if err := os.Mkdir(f.home, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(f.home, mode); err != nil {
				t.Fatal(err)
			}

			_, initErr := vault.Init(context.Background(), f.env(), vault.InitOptions{KeyFile: true})
			openErr := openErr(t, f)

			for op, err := range map[string]error{"Init": initErr, "Open": openErr} {
				if !errors.Is(err, vault.ErrInsecureHome) || !strings.Contains(err.Error(), "chmod 700") {
					t.Errorf("%s err = %v, want ErrInsecureHome with a chmod hint", op, err)
				}
			}
			assertMode(t, f.home, mode)
			if entries, _ := os.ReadDir(f.home); len(entries) != 0 {
				t.Errorf("files were created in an insecure home: %v", entries)
			}
		})
	}
}

func TestOpenRejectsAHomeThatBecameInsecure(t *testing.T) {
	_, f := newStore(t)
	if err := os.Chmod(f.home, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := openErr(t, f); !errors.Is(err, vault.ErrInsecureHome) {
		t.Fatalf("err = %v, want ErrInsecureHome", err)
	}
}

func TestOpenRejectsADatabaseOthersCanRead(t *testing.T) {
	_, f := newStore(t)
	if err := os.Chmod(filepath.Join(f.home, "addition.db"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := openErr(t, f); !errors.Is(err, vault.ErrInsecureHome) {
		t.Fatalf("err = %v, want ErrInsecureHome", err)
	}
}

func TestVaultFilesArePrivateUnderAPermissiveUmask(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	f := newFakeEnv(t)

	mustInit(t, f, vault.InitOptions{KeyFile: true})
	s := mustOpen(t, f)
	if _, err := s.CreateAccount(context.Background(), vault.Account{Provider: "google", Alias: "work"}); err != nil {
		t.Fatal(err)
	}

	assertMode(t, f.home, 0o700)
	for _, name := range []string{"addition.db", "addition.db-wal", "addition.db-shm", "master.key"} {
		assertMode(t, filepath.Join(f.home, name), 0o600)
	}
}

func TestKeyFileMustBeARegularFileNotALink(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"symlink to a 0600 file": func(t *testing.T, path string) {
			target := filepath.Join(privateTempDir(t), "real.key")
			content, _ := os.ReadFile(path)
			if err := os.WriteFile(target, content, 0o600); err != nil {
				t.Fatal(err)
			}
			_ = os.Remove(path)
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, path string) {
			_ = os.Remove(path)
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, path string) {
			_ = os.Remove(path)
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, replace := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeEnv(t)
			mustInit(t, f, vault.InitOptions{KeyFile: true})
			replace(t, filepath.Join(f.home, "master.key"))

			done := make(chan error, 1)
			go func() { done <- openErr(t, f) }()
			select {
			case err := <-done:
				if !errors.Is(err, vault.ErrInsecureKeyFile) {
					t.Errorf("err = %v, want ErrInsecureKeyFile", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Open blocked on the key file")
			}
		})
	}
}
