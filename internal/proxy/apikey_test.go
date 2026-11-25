package proxy_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/proxy"
	"github.com/runagent-dev/addition/internal/vault"
)

const pastedToken = "pk_proxy-pasted-token"

func apiKeyCredential() vault.Credential {
	return vault.Credential{AccessToken: pastedToken, Kind: vault.KindAPIKey}
}

func TestAnAPIKeyIsHandedOutAsIsWhateverTheClockSays(t *testing.T) {
	cred := apiKeyCredential()
	cred.ExpiresAt = t0.Add(-time.Hour)
	cred.RefreshToken = oldRefresh
	f := newVaultFixture(t, cred)
	m := f.manager(f.store)

	got, err := m.Access(context.Background())

	if err != nil || got != pastedToken {
		t.Fatalf("Access = %q, %v; want the stored token", got, err)
	}
	if f.endpoint.hits.Load() != 0 {
		t.Error("an api key was refreshed")
	}
	if m.AuthMethod() != vault.KindAPIKey {
		t.Errorf("AuthMethod = %q, want api_key", m.AuthMethod())
	}
}

func TestAnOAuthCredentialReportsTheOAuthMethod(t *testing.T) {
	f := newVaultFixture(t, expiringIn(time.Hour))
	m := f.manager(f.store)

	if _, err := m.Access(context.Background()); err != nil {
		t.Fatal(err)
	}

	if m.AuthMethod() != vault.KindOAuth2 {
		t.Errorf("AuthMethod = %q, want oauth2", m.AuthMethod())
	}
}

func TestARejectedAPIKeyMarksTheAccountNeedsReauthWithoutAnyRefresh(t *testing.T) {
	f := newVaultFixture(t, apiKeyCredential())
	m := f.manager(f.store)
	if _, err := m.Access(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err := m.Renew(context.Background(), pastedToken)

	if !errors.Is(err, proxy.ErrTokenRejected) || !errors.Is(err, proxy.ErrNeedsReauth) {
		t.Fatalf("err = %v, want ErrTokenRejected, which is also ErrNeedsReauth", err)
	}
	if f.status() != vault.AccountNeedsReauth {
		t.Errorf("status = %q, want needs-reauth", f.status())
	}
	if hits := f.endpoint.hits.Load(); hits != 0 {
		t.Errorf("token endpoint hits = %d, want 0: an api key has nothing to refresh", hits)
	}
	if cred := f.credential(); cred.AccessToken != pastedToken || cred.Kind != vault.KindAPIKey {
		t.Errorf("the credential changed: %+v", cred)
	}
	if _, err := f.manager(f.open()).Access(context.Background()); !errors.Is(err, proxy.ErrTokenRejected) {
		t.Errorf("Access after the rejection err = %v, want ErrTokenRejected so the caller can name the token command", err)
	}
}

func TestARejectedAPIKeyReplacedMeanwhileIsNotMarked(t *testing.T) {
	f := newVaultFixture(t, apiKeyCredential())
	m := f.manager(f.store)
	if _, err := m.Access(context.Background()); err != nil {
		t.Fatal(err)
	}
	other := f.open()
	if err := other.PutCredential(context.Background(), f.account.ID, vault.Credential{AccessToken: "pk_replacement", Kind: vault.KindAPIKey}); err != nil {
		t.Fatal(err)
	}

	got, err := m.Renew(context.Background(), pastedToken)

	if err != nil || got != "pk_replacement" {
		t.Fatalf("Renew = %q, %v; want the token another process stored", got, err)
	}
	if f.status() != vault.AccountActive {
		t.Errorf("status = %q, want active: the rejected token was already replaced", f.status())
	}
}

func apiKeyEngine(prefix *string, method string, s *sleeps) *proxy.Engine {
	e := newEngine(nil, s)
	e.API.APIKeyAuthPrefix = prefix
	e.AuthMethod = method
	return e
}

func ptr(s string) *string { return &s }

func TestTheAuthHeaderFollowsTheCredentialKind(t *testing.T) {
	cases := []struct {
		name       string
		apiKeyPref *string
		method     string
		want       string
	}{
		{"api key with an empty prefix is the bare token", ptr(""), vault.KindAPIKey, pastedToken},
		{"api key with its own prefix", ptr("Token"), vault.KindAPIKey, "Token " + pastedToken},
		{"api key with no prefix of its own uses the api one", nil, vault.KindAPIKey, "Bearer " + pastedToken},
		{"oauth ignores the api key prefix", ptr(""), vault.KindOAuth2, "Bearer " + pastedToken},
		{"a credential of no recorded kind is oauth", ptr(""), "", "Bearer " + pastedToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newUpstream(t, reply(http.StatusOK, `{}`))
			e := apiKeyEngine(tc.apiKeyPref, tc.method, &sleeps{})
			e.API.AllowedHosts = listedTestHosts()

			if _, _, err := do(t, e, proxy.Request{Method: "GET", URL: up.url("/me")}, pastedToken); err != nil {
				t.Fatal(err)
			}

			if got := up.seen()[0].auth; got != tc.want {
				t.Errorf("Authorization = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAnAPIAuthPrefixThatIsEmptyIsTheBareTokenWithoutALeadingSpace(t *testing.T) {
	up := newUpstream(t, reply(http.StatusOK, `{}`))
	e := newEngine(nil, &sleeps{})
	e.API = catalog.API{AuthHeader: "X-Api-Key", AuthPrefix: "", AllowedHosts: listedTestHosts()}

	if _, _, err := do(t, e, proxy.Request{Method: "GET", URL: up.url("/me")}, pastedToken); err != nil {
		t.Fatal(err)
	}

	if got := up.seen()[0].header.Get("X-Api-Key"); got != pastedToken {
		t.Errorf("X-Api-Key = %q, want the bare token", got)
	}
}

func TestARejectedAPIKeyIsNeverResentByTheEngine(t *testing.T) {
	up := newUpstream(t, reply(http.StatusUnauthorized, `{}`))
	renewer := &fakeRenewer{err: proxy.ErrTokenRejected}
	e := apiKeyEngine(ptr(""), vault.KindAPIKey, &sleeps{})
	e.Renewer = renewer
	e.API.AllowedHosts = listedTestHosts()

	_, _, err := do(t, e, proxy.Request{Method: "GET", URL: up.url("/me")}, pastedToken)

	if !errors.Is(err, proxy.ErrTokenRejected) {
		t.Fatalf("err = %v, want ErrTokenRejected", err)
	}
	if n := len(up.seen()); n != 1 || len(renewer.calls) != 1 {
		t.Errorf("requests = %d, renewals = %d; want one each", n, len(renewer.calls))
	}
}
