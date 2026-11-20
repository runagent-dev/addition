package vault_test

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/runagent-dev/addition/internal/vault"
)

func TestWithImmediateTxCommitsOrRollsBack(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	acct, _ := s.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work"})

	boom := errors.New("boom")
	err := s.WithImmediateTx(ctx, func(tx *vault.Tx) error {
		if err := tx.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: "discarded"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the callback's error", err)
	}
	if _, err := s.GetCredential(ctx, acct.ID); !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("rolled-back write is visible (err = %v)", err)
	}

	err = s.WithImmediateTx(ctx, func(tx *vault.Tx) error {
		return tx.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: "kept"})
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got, _ := s.GetCredential(ctx, acct.ID); got.AccessToken != "kept" {
		t.Errorf("committed credential = %q, want kept", got.AccessToken)
	}
}

// Each writer is its own Store, as separate addn processes would be.
func TestConcurrentImmediateTxsSerialize(t *testing.T) {
	const writers, rounds = 4, 15
	ctx := context.Background()
	f := newFakeEnv(t)
	f.set("ADDN_MASTER_KEY", strings.Repeat("d", 64))
	mustInit(t, f, vault.InitOptions{})
	setup := mustOpen(t, f)
	acct, _ := setup.CreateAccount(ctx, vault.Account{Provider: "google", Alias: "work"})
	if err := setup.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: "0"}); err != nil {
		t.Fatal(err)
	}

	stores := make([]*vault.Store, writers)
	for i := range stores {
		stores[i] = mustOpen(t, f)
	}

	var inside, overlaps atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, writers*rounds)
	for _, s := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				errs <- s.WithImmediateTx(ctx, func(tx *vault.Tx) error {
					if inside.Add(1) != 1 {
						overlaps.Add(1)
					}
					defer inside.Add(-1)

					cred, err := tx.GetCredential(ctx, acct.ID)
					if err != nil {
						return err
					}
					n, _ := strconv.Atoi(cred.AccessToken)
					runtime.Gosched()
					return tx.PutCredential(ctx, acct.ID, vault.Credential{AccessToken: strconv.Itoa(n + 1)})
				})
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("writer failed: %v", err)
		}
	}
	if n := overlaps.Load(); n != 0 {
		t.Errorf("%d transactions overlapped", n)
	}
	got, _ := setup.GetCredential(ctx, acct.ID)
	if want := strconv.Itoa(writers * rounds); got.AccessToken != want {
		t.Errorf("counter = %s, want %s (lost updates)", got.AccessToken, want)
	}
}

func TestWithImmediateTxHonoursCancellation(t *testing.T) {
	s, _ := newStore(t)
	acct, err := s.CreateAccount(context.Background(), vault.Account{Provider: "google", Alias: "work"})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := false
		err := s.WithImmediateTx(ctx, func(*vault.Tx) error { called = true; return nil })
		if !errors.Is(err, context.Canceled) || called {
			t.Fatalf("err = %v, called = %v; want context.Canceled without running fn", err, called)
		}
	})

	t.Run("cancelled during fn", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		err := s.WithImmediateTx(ctx, func(tx *vault.Tx) error {
			if err := tx.PutCredential(context.Background(), acct.ID, vault.Credential{AccessToken: "discarded"}); err != nil {
				return err
			}
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if _, err := s.GetCredential(context.Background(), acct.ID); !errors.Is(err, vault.ErrNotFound) {
			t.Fatalf("write from a cancelled transaction was committed (err = %v)", err)
		}
	})
}
