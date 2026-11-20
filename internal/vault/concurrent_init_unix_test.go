//go:build unix

package vault_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/runagent-dev/addition/internal/vault"
)

// A second operation that starts while init is publishing its key file must wait, not remove the temp name.
func TestKeyTempCleanupWaitsForALivePublication(t *testing.T) {
	seconds := map[string]func(f *fakeEnv) error{
		"Init": func(f *fakeEnv) error {
			res, err := vault.Init(context.Background(), f.env(), vault.InitOptions{KeyFile: true})
			if err == nil && res.Created {
				t.Error("the second init created a second vault")
			}
			return err
		},
		"Open": func(f *fakeEnv) error { return openErr(t, f) },
	}
	for name, second := range seconds {
		t.Run(name, func(t *testing.T) {
			f := newFakeEnv(t)
			var publishing atomic.Bool
			var once sync.Once
			waitingForLock := make(chan struct{})
			secondDone := make(chan error, 1)
			t.Cleanup(vault.SetBeforeWriterLock(func() {
				if publishing.Load() {
					once.Do(func() { close(waitingForLock) })
				}
			}))
			t.Cleanup(vault.SetBeforeKeyPublish(func() {
				if publishing.Swap(true) {
					return
				}
				go func() { secondDone <- second(f) }()
				select {
				case <-waitingForLock:
				case err := <-secondDone:
					secondDone <- err
				}
			}))

			_, firstErr := vault.Init(context.Background(), f.env(), vault.InitOptions{KeyFile: true})
			secondErr := <-secondDone

			if firstErr != nil {
				t.Errorf("publishing init: %v", firstErr)
			}
			if secondErr != nil {
				t.Errorf("concurrent %s: %v", name, secondErr)
			}
			mustOpen(t, f)
		})
	}
}
