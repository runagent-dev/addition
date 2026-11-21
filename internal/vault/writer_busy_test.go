package vault_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/vault"
)

// holdWriterLock keeps the vault's writer lock on s until the returned release is called.
func holdWriterLock(t *testing.T, s *vault.Store) (release func()) {
	t.Helper()
	holding, done, stop := make(chan struct{}), make(chan error, 1), make(chan struct{})
	go func() {
		done <- s.WithImmediateTx(context.Background(), func(*vault.Tx) error {
			close(holding)
			<-stop
			return nil
		})
	}()
	<-holding
	var once bool
	return func() {
		if !once {
			once = true
			close(stop)
			if err := <-done; err != nil {
				t.Errorf("lock holder: %v", err)
			}
		}
	}
}

func TestWriterLockContentionIsAStableError(t *testing.T) {
	restore := vault.SetBusyTimeout(50 * time.Millisecond)
	defer restore()
	holder, f := newStore(t)
	contender := mustOpen(t, f)
	release := holdWriterLock(t, holder)
	defer release()

	err := contender.WithImmediateTx(context.Background(), func(*vault.Tx) error { return nil })

	if !errors.Is(err, vault.ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "sqlite") || strings.Contains(err.Error(), "(5)") {
		t.Errorf("the error leaks the driver's text: %v", err)
	}
	if _, err := vault.Open(context.Background(), f.env()); !errors.Is(err, vault.ErrBusy) {
		t.Errorf("Open while the lock is held err = %v, want ErrBusy", err)
	}
}

func TestWaitingForTheWriterLockHonoursCancellation(t *testing.T) {
	holder, f := newStore(t)
	contender := mustOpen(t, f)
	release := holdWriterLock(t, holder)
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	waited, waiting := make(chan error, 1), make(chan struct{})
	var once sync.Once
	defer vault.SetBeforeWriterLock(func() { once.Do(func() { close(waiting) }) })()
	go func() {
		waited <- contender.WithImmediateTx(ctx, func(*vault.Tx) error {
			t.Error("the callback ran without the lock")
			return nil
		})
	}()

	<-waiting
	cancel()

	select {
	case err := <-waited:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled waiter is still blocked on the writer lock")
	}
	release()
	if err := contender.WithImmediateTx(context.Background(), func(*vault.Tx) error { return nil }); err != nil {
		t.Errorf("the abandoned acquisition left the lock held: %v", err)
	}
}

func TestAuditWithinDeadlineGivesUpWhenTheDeadlinePasses(t *testing.T) {
	holder, f := newStore(t)
	acct, err := holder.CreateAccount(context.Background(), vault.Account{Provider: "google", Alias: "work"})
	if err != nil {
		t.Fatal(err)
	}
	contender := mustOpen(t, f)
	release := holdWriterLock(t, holder)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	err = contender.AuditWithinDeadline(ctx, vault.CallEvent{AccountID: acct.ID, Method: "GET", Status: 204})

	if err == nil || time.Since(started) > 3*time.Second {
		t.Errorf("err = %v after %v; want a failure about when the deadline passed", err, time.Since(started))
	}
}

func TestOpenWaitingForTheWriterLockHonoursCancellation(t *testing.T) {
	holder, f := newStore(t)
	release := holdWriterLock(t, holder)
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	opened, waiting := make(chan error, 1), make(chan struct{})
	var once sync.Once
	defer vault.SetBeforeWriterLock(func() { once.Do(func() { close(waiting) }) })()
	go func() {
		s, err := vault.Open(ctx, f.env())
		if s != nil {
			_ = s.Close()
		}
		opened <- err
	}()

	<-waiting
	cancel()

	select {
	case err := <-opened:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled Open is still blocked on the writer lock")
	}
	release()
	if _, err := vault.Open(context.Background(), f.env()); err != nil {
		t.Errorf("the abandoned Open left the lock held: %v", err)
	}
}
