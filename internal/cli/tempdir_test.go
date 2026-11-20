package cli

import (
	"os"
	"testing"
)

// privateTempDir is a 0700 temp dir: t.TempDir follows the umask, and a vault home refuses ancestors others can write.
func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
