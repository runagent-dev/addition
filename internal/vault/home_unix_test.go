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

	"github.com/runagent-dev/addition/internal/vault"
)

// bothErrors runs Init and then Open against f and returns their errors.
func bothErrors(t *testing.T, f *fakeEnv) map[string]error {
	t.Helper()
	_, initErr := vault.Init(context.Background(), f.env(), vault.InitOptions{KeyFile: true})
	return map[string]error{"Init": initErr, "Open": openErr(t, f)}
}

func TestRejectsAHomeUnderAnAncestorOthersCanWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns every directory, so the ownership rules cannot be exercised")
	}
	for _, mode := range []os.FileMode{0o777, 0o775, 0o757, os.ModeSticky | 0o777} {
		t.Run(mode.String(), func(t *testing.T) {
			f := newFakeEnv(t)
			shared := filepath.Join(privateTempDir(t), "shared")
			if err := os.Mkdir(shared, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(shared, mode); err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(shared, "home")
			f.set("ADDN_HOME", home)

			for op, err := range bothErrors(t, f) {
				if !errors.Is(err, vault.ErrInsecureHome) || !strings.Contains(err.Error(), shared) {
					t.Errorf("%s err = %v, want ErrInsecureHome naming %s", op, err, shared)
				}
			}
			if _, err := os.Stat(filepath.Join(home, "addition.db")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the database was created under an insecure ancestor (stat err = %v)", err)
			}
		})
	}
}

// A root-owned sticky ancestor cannot be made without root; the system temp dir stands in for it.
func TestAllowsAStickyRootOwnedAncestor(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(tmp)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	if st.Uid != 0 || info.Mode()&os.ModeSticky == 0 || info.Mode().Perm()&0o002 == 0 {
		t.Skipf("%s is not a root-owned, sticky, world-writable directory", tmp)
	}
	parent, err := os.MkdirTemp(tmp, "addn-sticky-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	f := newFakeEnv(t)
	f.set("ADDN_HOME", filepath.Join(parent, "home"))

	mustInit(t, f, vault.InitOptions{KeyFile: true})
	mustOpen(t, f)
}

func TestResolvesAHomeBehindASymlink(t *testing.T) {
	f := newFakeEnv(t)
	base := privateTempDir(t)
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	f.set("ADDN_HOME", link)

	res := mustInit(t, f, vault.InitOptions{KeyFile: true})
	mustOpen(t, f)

	want, _ := filepath.EvalSymlinks(real)
	if res.Home != want {
		t.Errorf("home = %q, want the resolved %q", res.Home, want)
	}
	if _, err := os.Lstat(filepath.Join(real, "addition.db")); err != nil {
		t.Errorf("database not in the resolved home: %v", err)
	}
}

func TestValidatesTheAncestorsOfASymlinkTarget(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns every directory, so the ownership rules cannot be exercised")
	}
	f := newFakeEnv(t)
	shared := filepath.Join(privateTempDir(t), "shared")
	if err := os.MkdirAll(filepath.Join(shared, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(privateTempDir(t), "link")
	if err := os.Symlink(filepath.Join(shared, "home"), link); err != nil {
		t.Fatal(err)
	}
	f.set("ADDN_HOME", link)

	for op, err := range bothErrors(t, f) {
		if !errors.Is(err, vault.ErrInsecureHome) || !strings.Contains(err.Error(), shared) {
			t.Errorf("%s err = %v, want ErrInsecureHome naming %s", op, err, shared)
		}
	}
}

func TestRejectsInsecureSideFiles(t *testing.T) {
	seeds := map[string]func(t *testing.T, path string){
		"0644": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("stale shared memory"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, path string) {
			target := filepath.Join(privateTempDir(t), "target")
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for _, side := range []string{"-shm", "-wal", "-journal"} {
		for name, seed := range seeds {
			t.Run("fresh "+side+" "+name, func(t *testing.T) {
				f := newFakeEnv(t)
				if err := os.Mkdir(f.home, 0o700); err != nil {
					t.Fatal(err)
				}
				seed(t, filepath.Join(f.home, "addition.db"+side))

				_, err := vault.Init(context.Background(), f.env(), vault.InitOptions{KeyFile: true})
				if !errors.Is(err, vault.ErrInsecureHome) || !strings.Contains(err.Error(), "addition.db"+side) {
					t.Errorf("Init err = %v, want ErrInsecureHome naming the side file", err)
				}
				if _, err := os.Stat(filepath.Join(f.home, "addition.db")); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the database was created beside an insecure side file (stat err = %v)", err)
				}
			})
			t.Run("existing "+side+" "+name, func(t *testing.T) {
				f := newFakeEnv(t)
				mustInit(t, f, vault.InitOptions{KeyFile: true})
				path := filepath.Join(f.home, "addition.db"+side)
				_ = os.Remove(path)
				seed(t, path)

				for op, err := range bothErrors(t, f) {
					if !errors.Is(err, vault.ErrInsecureHome) {
						t.Errorf("%s err = %v, want ErrInsecureHome", op, err)
					}
				}
			})
		}
	}
}

// keyTemps lists the temporary names key publication uses.
func keyTemps(t *testing.T, home string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home, ".master.key-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestInitLeavesNoTemporaryKeyName(t *testing.T) {
	f := newFakeEnv(t)
	mustInit(t, f, vault.InitOptions{KeyFile: true})

	if temps := keyTemps(t, f.home); len(temps) != 0 {
		t.Errorf("temporary key names left behind: %v", temps)
	}
}

func TestInterruptedKeyPublicationIsCleanedOnRetry(t *testing.T) {
	for _, op := range []string{"Init", "Open"} {
		t.Run(op, func(t *testing.T) {
			f := newFakeEnv(t)
			mustInit(t, f, vault.InitOptions{KeyFile: true})
			temp := filepath.Join(f.home, ".master.key-interrupted")
			if err := os.Link(filepath.Join(f.home, "master.key"), temp); err != nil {
				t.Fatal(err)
			}

			if op == "Init" {
				mustInit(t, f, vault.InitOptions{KeyFile: true})
			} else {
				mustOpen(t, f)
			}

			if temps := keyTemps(t, f.home); len(temps) != 0 {
				t.Errorf("second name for the key survived: %v", temps)
			}
			if info, err := os.Stat(filepath.Join(f.home, "master.key")); err != nil {
				t.Errorf("master.key: %v", err)
			} else if n := info.Sys().(*syscall.Stat_t).Nlink; n != 1 {
				t.Errorf("master.key has %d links, want 1", n)
			}
		})
	}
}

func TestKeyTempCleanupLeavesForeignEntries(t *testing.T) {
	f := newFakeEnv(t)
	mustInit(t, f, vault.InitOptions{KeyFile: true})
	dir := filepath.Join(f.home, ".master.key-dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(privateTempDir(t), "outside")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.home, ".master.key-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	mustOpen(t, f)

	for _, path := range []string{dir, link, target} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("%s was removed: %v", path, err)
		}
	}
}
