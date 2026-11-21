package proxy_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/vault"
)

func TestRefreshCancelledBeforeTheExchangeNeverStartsIt(t *testing.T) {
	f := newVaultFixture(t, vault.Credential{AccessToken: oldAccess, RefreshToken: oldRefresh, ExpiresAt: t0.Add(-time.Hour)})
	ctx, cancel := context.WithCancel(context.Background())
	m := f.manager(f.store)
	m.BeforeExchange = cancel

	_, err := m.Access(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if n := f.endpoint.hits.Load(); n != 0 {
		t.Errorf("the token endpoint saw %d requests; a cancelled refresh must not rotate the grant", n)
	}
	if got := f.credential(); got.RefreshToken != oldRefresh {
		t.Errorf("refresh token = %q, want it untouched", got.RefreshToken)
	}
}
