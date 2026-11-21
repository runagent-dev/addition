package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCallOutputFileIsPrivate(t *testing.T) {
	c := newCallHarness(t)
	target := filepath.Join(t.TempDir(), "out.json")

	c.mustRun("call", "gmail", "-a", "work", "GET", "/x", "-o", target)

	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v; want 0600", info.Mode(), err)
	}
}

func TestCallNeverWritesThroughASymlinkAtTheTarget(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	_ = os.WriteFile(victim, []byte("precious"), 0o600)
	for name, link := range map[string]string{"to a file": victim, "dangling": filepath.Join(dir, "gone")} {
		for _, extra := range [][]string{nil, {"--force"}} {
			t.Run(name+" "+filepath.Join(extra...), func(t *testing.T) {
				c := newCallHarness(t)
				target := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(link, target); err != nil {
					t.Fatal(err)
				}

				r := c.call(append([]string{"-a", "work", "GET", "/x", "-o", target}, extra...)...)

				c.wantError(r, ExitUsage, "target-is-symlink")
				if data, _ := os.ReadFile(victim); string(data) != "precious" {
					t.Errorf("the link's destination was written: %q", data)
				}
				if _, err := os.Lstat(filepath.Join(dir, "gone")); err == nil {
					t.Error("a dangling link's destination was created")
				}
				if len(c.api.requests()) != 0 {
					t.Error("a request was sent")
				}
			})
		}
	}
}
