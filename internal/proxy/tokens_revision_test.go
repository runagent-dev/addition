package proxy_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/proxy"
	"github.com/runagent-dev/addition/internal/vault"
)

// lockHolder holds the vault's writer lock until release is called, then commits cred.
type lockHolder struct {
	release func()
	done    <-chan error
}

func (f *vaultFixture) holdWriterLock(commit vault.Credential) lockHolder {
	f.t.Helper()
	ctx := context.Background()
	holding, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- f.store.WithImmediateTx(ctx, func(tx *vault.Tx) error {
			close(holding)
			<-release
			return tx.PutCredential(ctx, f.account.ID, commit)
		})
	}()
	<-holding
	return lockHolder{release: func() { close(release) }, done: done}
}

// waitingOnTheLock runs fn on m in the background; observed closes when m has read its credential
// and is about to wait for the writer lock, so a holder released after that cannot be missed.
func waitingOnTheLock(m *proxy.TokenManager, fn func(*proxy.TokenManager) (string, error)) (observed <-chan struct{}, result <-chan error, token *string) {
	reached, out, got := make(chan struct{}), make(chan error, 1), new(string)
	m.BeforeWriterLock = func() { close(reached) }
	go func() {
		var err error
		*got, err = fn(m)
		out <- err
	}()
	return reached, out, got
}

func TestAWaitingRefresherUsesWhatTheLockHolderCommitted(t *testing.T) {
	f := newVaultFixture(t, expiringIn(-time.Minute))
	waiter := f.manager(f.open())
	holder := f.holdWriterLock(vault.Credential{AccessToken: newAccess, RefreshToken: oldRefresh, ExpiresAt: t0.Add(time.Hour)})
	observed, result, token := waitingOnTheLock(waiter, func(m *proxy.TokenManager) (string, error) { return m.Access(context.Background()) })
	<-observed

	holder.release()
	if err := <-holder.done; err != nil {
		t.Fatal(err)
	}

	if err := <-result; err != nil || *token != newAccess {
		t.Errorf("waiter got %q, %v; want the token the lock holder committed", *token, err)
	}
	if hits := f.endpoint.hits.Load(); hits != 0 {
		t.Errorf("token endpoint hits = %d, want 0", hits)
	}
}

func TestAWaiterReusesACommittedRefreshEvenWhenItIsAlreadyInsideTheSkew(t *testing.T) {
	f := newVaultFixture(t, expiringIn(-time.Minute))
	shortLived := vault.Credential{AccessToken: newAccess, RefreshToken: oldRefresh, ExpiresAt: t0.Add(30 * time.Second)}
	waiter := f.manager(f.open())
	holder := f.holdWriterLock(shortLived)
	observed, result, token := waitingOnTheLock(waiter, func(m *proxy.TokenManager) (string, error) { return m.Access(context.Background()) })
	<-observed

	holder.release()
	if err := <-holder.done; err != nil {
		t.Fatal(err)
	}

	if err := <-result; err != nil || *token != newAccess {
		t.Errorf("waiter got %q, %v; want the committed token", *token, err)
	}
	if hits := f.endpoint.hits.Load(); hits != 0 {
		t.Errorf("token endpoint hits = %d, want 0: a short-lived grant is still someone else's refresh", hits)
	}
}

func TestAWaiterReusesARotationThatReturnedTheSameAccessToken(t *testing.T) {
	f := newVaultFixture(t, expiringIn(time.Hour))
	sameTextRotated := vault.Credential{AccessToken: oldAccess, RefreshToken: newRefresh, ExpiresAt: t0.Add(time.Hour)}
	waiting := f.manager(f.open())
	if token, err := waiting.Access(context.Background()); err != nil || token != oldAccess {
		t.Fatalf("Access = %q, %v", token, err)
	}
	holder := f.holdWriterLock(sameTextRotated)
	reached, result, renewed := waitingOnTheLock(waiting, func(m *proxy.TokenManager) (string, error) {
		return m.Renew(context.Background(), oldAccess)
	})
	<-reached

	holder.release()
	if err := <-holder.done; err != nil {
		t.Fatal(err)
	}

	if err := <-result; err != nil || *renewed != oldAccess {
		t.Errorf("Renew = %q, %v", *renewed, err)
	}
	if hits := f.endpoint.hits.Load(); hits != 0 {
		t.Errorf("token endpoint hits = %d, want 0: the refresh token had already been rotated", hits)
	}
}

func TestTwoProcessesRenewingTheSameRejectedTokenHitTheEndpointOnce(t *testing.T) {
	f := newVaultFixture(t, expiringIn(time.Hour))
	f.endpoint.reply = func(int, url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": oldAccess, "refresh_token": newRefresh, "expires_in": 3600}
	}
	first, second := f.manager(f.open()), f.manager(f.open())
	for _, m := range []*proxy.TokenManager{first, second} {
		if _, err := m.Access(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	for _, m := range []*proxy.TokenManager{first, second} {
		if token, err := m.Renew(context.Background(), oldAccess); err != nil || token != oldAccess {
			t.Fatalf("Renew = %q, %v", token, err)
		}
	}

	if hits := f.endpoint.hits.Load(); hits != 1 {
		t.Errorf("token endpoint hits = %d, want 1: the provider answered with the same access token text, which is not a reason to refresh again", hits)
	}
}
