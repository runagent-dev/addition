package vault_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/runagent-dev/addition/internal/vault"
)

// cancelAtWriterLock cancels ctx as the nth writer lock is being taken: 1 is migrate, 2 is Init's own.
func cancelAtWriterLock(t *testing.T, n int) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	calls := 0
	t.Cleanup(vault.SetBeforeWriterLock(func() {
		if calls++; calls == n {
			cancel()
		}
	}))
	return ctx
}

func TestInitOnceItHoldsTheLockRunsToCompletion(t *testing.T) {
	f := newFakeEnv(t)
	ctx := cancelAtWriterLock(t, 2)

	res, err := vault.Init(ctx, f.env(), vault.InitOptions{KeyFile: true})
	if err != nil || !res.Created {
		t.Fatalf("Init = %+v, %v; want a created vault despite the late cancellation", res, err)
	}
	mustOpen(t, f)
}

func TestInitCancelledBeforeItsLockPublishesNothing(t *testing.T) {
	f := newFakeEnv(t)
	ctx := cancelAtWriterLock(t, 1)

	if _, err := vault.Init(ctx, f.env(), vault.InitOptions{KeyFile: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(filepath.Join(f.home, "master.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("master.key exists after a cancelled Init (err = %v)", err)
	}
	if res := mustInit(t, f, vault.InitOptions{KeyFile: true}); !res.Created {
		t.Fatal("retry did not create the vault")
	}
}
