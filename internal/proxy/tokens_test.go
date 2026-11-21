package proxy_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/oauth"
	"github.com/runagent-dev/addition/internal/proxy"
	"github.com/runagent-dev/addition/internal/vault"
)

func expiringIn(d time.Duration) vault.Credential {
	return vault.Credential{AccessToken: oldAccess, RefreshToken: oldRefresh, ExpiresAt: t0.Add(d)}
}

func TestAccessReturnsAFreshTokenWithoutTouchingTheNetwork(t *testing.T) {
	f := newVaultFixture(t, expiringIn(time.Hour))

	got, err := f.manager(f.store).Access(context.Background())

	if err != nil || got != oldAccess {
		t.Fatalf("Access = %q, %v; want the stored token", got, err)
	}
	if f.endpoint.hits.Load() != 0 {
		t.Error("a fresh token triggered a refresh")
	}
}

func TestAccessRefreshesInsideTheSkewWindow(t *testing.T) {
	cases := []struct {
		name      string
		expiresIn time.Duration
		refreshes bool
	}{
		{"already expired", -time.Minute, true},
		{"expires within the skew", 30 * time.Second, true},
		{"expires exactly at the skew", proxy.AccessTokenSkew, true},
		{"expires just past the skew", proxy.AccessTokenSkew + time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newVaultFixture(t, expiringIn(tc.expiresIn))

			got, err := f.manager(f.store).Access(context.Background())

			if err != nil {
				t.Fatalf("Access: %v", err)
			}
			if want := map[bool]string{true: newAccess, false: oldAccess}[tc.refreshes]; got != want {
				t.Errorf("token = %q, want %q", got, want)
			}
			if hits := f.endpoint.hits.Load(); (hits == 1) != tc.refreshes || hits > 1 {
				t.Errorf("token endpoint hits = %d, refreshes expected: %v", hits, tc.refreshes)
			}
		})
	}
	if proxy.AccessTokenSkew != 60*time.Second {
		t.Errorf("AccessTokenSkew = %s, want 60s", proxy.AccessTokenSkew)
	}
}

func TestAccessNeverRefreshesATokenWithoutAnExpiry(t *testing.T) {
	f := newVaultFixture(t, vault.Credential{AccessToken: oldAccess})
	f.clock.Advance(1000 * time.Hour)

	got, err := f.manager(f.store).Access(context.Background())

	if err != nil || got != oldAccess || f.endpoint.hits.Load() != 0 {
		t.Errorf("Access = %q, %v with %d hits; want the stored token and no refresh", got, err, f.endpoint.hits.Load())
	}
}

func TestRefreshStoresTheNewTokenExpiryAndKeepsTheRefreshToken(t *testing.T) {
	f := newVaultFixture(t, expiringIn(-time.Minute))

	if _, err := f.manager(f.store).Access(context.Background()); err != nil {
		t.Fatalf("Access: %v", err)
	}

	cred := f.credential()
	if cred.AccessToken != newAccess || cred.RefreshToken != oldRefresh || !cred.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("credential = %+v, want the new access token, the old refresh token and expiry t0+1h", cred)
	}
	form := f.endpoint.form(0)
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != oldRefresh ||
		form.Get("client_id") != clientID || form.Get("client_secret") != clientSecret {
		t.Errorf("token request = %v", form)
	}
	if got := f.auditDetails("refresh"); len(got) != 1 || got[0] != `{"outcome":"ok"}` {
		t.Errorf("refresh audit rows = %v", got)
	}
}

func TestRefreshStoresARotatedRefreshToken(t *testing.T) {
	f := newVaultFixture(t, expiringIn(-time.Minute))
	f.endpoint.reply = func(int, url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": newAccess, "refresh_token": newRefresh, "expires_in": 3600}
	}

	if _, err := f.manager(f.store).Access(context.Background()); err != nil {
		t.Fatalf("Access: %v", err)
	}

	if cred := f.credential(); cred.RefreshToken != newRefresh {
		t.Errorf("refresh token = %q, want the rotated one", cred.RefreshToken)
	}
}

func TestInvalidGrantMarksTheAccountNeedsReauthAndNeverRetries(t *testing.T) {
	f := newVaultFixture(t, expiringIn(-time.Minute))
	f.endpoint.reply = func(int, url.Values) (int, map[string]any) {
		return 400, map[string]any{"error": "invalid_grant", "error_description": oldRefresh}
	}
	m := f.manager(f.store)

	_, err := m.Access(context.Background())

	if !errors.Is(err, proxy.ErrNeedsReauth) {
		t.Fatalf("err = %v, want ErrNeedsReauth", err)
	}
	if f.status() != vault.AccountNeedsReauth {
		t.Errorf("status = %q, want needs-reauth", f.status())
	}
	if got := f.auditDetails("refresh"); len(got) != 1 || got[0] != `{"outcome":"needs-reauth"}` {
		t.Errorf("refresh audit rows = %v", got)
	}
	if cred := f.credential(); cred.AccessToken != oldAccess || cred.RefreshToken != oldRefresh {
		t.Errorf("a failed refresh changed the credential: %+v", cred)
	}
	if strings.Contains(err.Error(), oldRefresh) {
		t.Errorf("the error repeats the refresh token: %v", err)
	}

	if _, err := m.Access(context.Background()); !errors.Is(err, proxy.ErrNeedsReauth) {
		t.Errorf("second Access err = %v, want ErrNeedsReauth", err)
	}
	if _, err := f.manager(f.open()).Renew(context.Background(), oldAccess); !errors.Is(err, proxy.ErrNeedsReauth) {
		t.Errorf("Renew err = %v, want ErrNeedsReauth", err)
	}
	if hits := f.endpoint.hits.Load(); hits != 1 {
		t.Errorf("token endpoint hits = %d, want exactly 1: a dead refresh token is never retried", hits)
	}
}

func TestOtherRefreshFailuresKeepTheAccountActive(t *testing.T) {
	cases := map[string]func(*vaultFixture){
		"server error": func(f *vaultFixture) {
			f.endpoint.reply = func(int, url.Values) (int, map[string]any) { return 500, map[string]any{"error": "server_error"} }
		},
		"invalid_client": func(f *vaultFixture) {
			f.endpoint.reply = func(int, url.Values) (int, map[string]any) { return 401, map[string]any{"error": "invalid_client"} }
		},
		"network": func(f *vaultFixture) { f.endpoint.srv.Close() },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			f := newVaultFixture(t, expiringIn(-time.Minute))
			breakIt(f)

			_, err := f.manager(f.store).Access(context.Background())

			var perr *oauth.ProviderError
			var nerr *oauth.NetworkError
			if err == nil || errors.Is(err, proxy.ErrNeedsReauth) || !(errors.As(err, &perr) || errors.As(err, &nerr)) {
				t.Fatalf("err = %#v, want the provider or network error", err)
			}
			if f.status() != vault.AccountActive {
				t.Errorf("status = %q, want active", f.status())
			}
			if got := f.auditDetails("refresh"); len(got) != 1 || got[0] != `{"outcome":"failed"}` {
				t.Errorf("refresh audit rows = %v", got)
			}
			if f.credential().AccessToken != oldAccess {
				t.Error("a failed refresh changed the credential")
			}
		})
	}
}

func TestRefreshWithoutARefreshTokenOrClientOrCredential(t *testing.T) {
	t.Run("no refresh token", func(t *testing.T) {
		f := newVaultFixture(t, vault.Credential{AccessToken: oldAccess, ExpiresAt: t0.Add(-time.Minute)})

		_, err := f.manager(f.store).Access(context.Background())

		if !errors.Is(err, proxy.ErrNoRefreshToken) || f.endpoint.hits.Load() != 0 {
			t.Errorf("err = %v with %d hits, want ErrNoRefreshToken and no request", err, f.endpoint.hits.Load())
		}
	})
	t.Run("no OAuth client", func(t *testing.T) {
		f := newVaultFixture(t, expiringIn(-time.Minute))
		if _, err := f.rawDB().Exec(`DELETE FROM oauth_clients`); err != nil {
			t.Fatal(err)
		}

		_, err := f.manager(f.store).Access(context.Background())

		if !errors.Is(err, proxy.ErrNoOAuthClient) || f.endpoint.hits.Load() != 0 {
			t.Errorf("err = %v with %d hits, want ErrNoOAuthClient and no request", err, f.endpoint.hits.Load())
		}
	})
	t.Run("no credential", func(t *testing.T) {
		f := newVaultFixture(t, expiringIn(time.Hour))
		if _, err := f.rawDB().Exec(`DELETE FROM credentials`); err != nil {
			t.Fatal(err)
		}

		_, err := f.manager(f.store).Access(context.Background())

		if !errors.Is(err, proxy.ErrNoCredential) {
			t.Errorf("err = %v, want ErrNoCredential", err)
		}
	})
}

func TestRenewRefreshesOnlyTheTokenThatWasRejected(t *testing.T) {
	f := newVaultFixture(t, expiringIn(time.Hour))

	got, err := f.manager(f.store).Renew(context.Background(), oldAccess)

	if err != nil || got != newAccess || f.endpoint.hits.Load() != 1 {
		t.Fatalf("Renew = %q, %v with %d hits; want a refresh", got, err, f.endpoint.hits.Load())
	}
	other := f.manager(f.open())
	got, err = other.Renew(context.Background(), oldAccess)
	if err != nil || got != newAccess || f.endpoint.hits.Load() != 1 {
		t.Errorf("Renew after another process refreshed = %q, %v with %d hits; want the stored token and no second refresh", got, err, f.endpoint.hits.Load())
	}
}

func TestRenewWithoutARefreshToken(t *testing.T) {
	f := newVaultFixture(t, vault.Credential{AccessToken: oldAccess, ExpiresAt: t0.Add(time.Hour)})

	_, err := f.manager(f.store).Renew(context.Background(), oldAccess)

	if !errors.Is(err, proxy.ErrNoRefreshToken) {
		t.Errorf("err = %v, want ErrNoRefreshToken", err)
	}
}

func TestConcurrentProcessesRefreshOnce(t *testing.T) {
	f := newVaultFixture(t, expiringIn(-time.Minute))
	const processes = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	tokens := make([]string, processes)
	errs := make([]error, processes)
	for i := range processes {
		m := f.manager(f.open())
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tokens[i], errs[i] = m.Access(context.Background())
		}()
	}
	close(start)
	wg.Wait()

	for i := range processes {
		if errs[i] != nil || tokens[i] != newAccess {
			t.Errorf("process %d: token %q, err %v", i, tokens[i], errs[i])
		}
	}
	if hits := f.endpoint.hits.Load(); hits != 1 {
		t.Errorf("token endpoint hits = %d, want exactly 1", hits)
	}
}

func TestAWaitingRefresherUsesWhatTheLockHolderCommitted(t *testing.T) {
	f := newVaultFixture(t, expiringIn(-time.Minute))
	ctx := context.Background()
	holding, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- f.store.WithImmediateTx(ctx, func(tx *vault.Tx) error {
			close(holding)
			<-release
			return tx.PutCredential(ctx, f.account.ID, vault.Credential{AccessToken: newAccess, RefreshToken: oldRefresh, ExpiresAt: t0.Add(time.Hour)})
		})
	}()
	<-holding
	got := make(chan string, 1)
	waiter := f.manager(f.open())
	go func() {
		token, err := waiter.Access(ctx)
		if err != nil {
			token = "error: " + err.Error()
		}
		got <- token
	}()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if token := <-got; token != newAccess {
		t.Errorf("waiter got %q, want the token the lock holder committed", token)
	}
	if hits := f.endpoint.hits.Load(); hits != 0 {
		t.Errorf("token endpoint hits = %d, want 0", hits)
	}
}

func TestRotatedRefreshTokenIsUsedForTheNextRefresh(t *testing.T) {
	f := newVaultFixture(t, expiringIn(-time.Minute))
	f.endpoint.reply = func(n int, form url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": newAccess, "refresh_token": newRefresh, "expires_in": 3600}
	}
	m := f.manager(f.store)
	if _, err := m.Access(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(2 * time.Hour)

	if _, err := m.Access(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := f.endpoint.form(1).Get("refresh_token"); got != newRefresh {
		t.Errorf("the second refresh sent %q, want the rotated token", got)
	}
}
