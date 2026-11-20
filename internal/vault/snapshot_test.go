package vault_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/runagent-dev/addition/internal/vault"
)

func TestOpenReadsInitRecordsFromOneSnapshot(t *testing.T) {
	f := newFakeEnv(t)
	if err := os.Mkdir(f.home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, "addition.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	committed := false
	t.Cleanup(vault.SetAfterFirstInitRecord(func() {
		if !committed {
			committed = true
			mustInit(t, f, vault.InitOptions{KeyFile: true})
		}
	}))

	err := openErr(t, f)

	if !committed {
		t.Fatal("the concurrent init never ran")
	}
	if errors.Is(err, vault.ErrCorrupt) {
		t.Fatalf("Open reported a committed init as corrupt: %v", err)
	}
	mustOpen(t, f)
}
