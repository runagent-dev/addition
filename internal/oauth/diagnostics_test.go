package oauth_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/oauth"
)

// echoingServer answers every request with status and an error body that repeats what it was sent.
func echoingServer(t *testing.T, status int, code string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		echo := string(body) + " " + r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":%q,"error_description":%q,"error_uri":%q}`, code, echo, "https://x.example/?"+echo)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// backChannelCalls runs each OAuth back-channel request against p with hc.
func backChannelCalls(p *catalog.Provider, a oauth.Authorization) map[string]func(*http.Client) error {
	return map[string]func(*http.Client) error{
		"exchange": func(hc *http.Client) error {
			_, err := oauth.Exchange(context.Background(), hc, p, client, a, "the-auth-code")
			return err
		},
		"identity": func(hc *http.Client) error {
			_, err := oauth.FetchIdentity(context.Background(), hc, p, accessToken)
			return err
		},
		"revoke": func(hc *http.Client) error { return oauth.Revoke(context.Background(), hc, p, refreshToken) },
	}
}

func pointedAt(base string) *catalog.Provider {
	p := googleLike(base)
	p.OAuth2.RevokeURL = base
	p.OAuth2.Identity.URL = base
	return p
}

func assertNoSecretIn(t *testing.T, msg string, a oauth.Authorization) {
	t.Helper()
	for name, s := range map[string]string{
		"client secret": clientSecret, "verifier": a.Verifier, "code": "the-auth-code",
		"access token": accessToken, "refresh token": refreshToken, "echoed marker": "echo-marker",
	} {
		if strings.Contains(msg, s) {
			t.Errorf("the error message carries the %s", name)
		}
	}
}

func TestProviderDiagnosticsAreLocallyAuthored(t *testing.T) {
	cases := []struct {
		code     string
		wantCode string
	}{
		{"invalid_grant", "invalid_grant"},
		{"invalid_client", "invalid_client"},
		{"echo-marker " + clientSecret, ""},
	}
	for _, tc := range cases {
		srv := echoingServer(t, http.StatusBadRequest, tc.code)
		p := pointedAt(srv.URL)
		a := newAuthorization(t, p.OAuth2, "openid")
		for name, call := range backChannelCalls(p, a) {
			t.Run(name+"/"+tc.wantCode, func(t *testing.T) {
				err := call(srv.Client())
				var perr *oauth.ProviderError
				if !errors.As(err, &perr) || perr.Status != 400 || perr.Code != tc.wantCode {
					t.Fatalf("err = %#v, want ProviderError 400 code %q", err, tc.wantCode)
				}
				msg := err.Error()
				assertNoSecretIn(t, msg, a)
				if !strings.Contains(msg, "HTTP 400") || (tc.wantCode != "" && !strings.Contains(msg, tc.wantCode)) {
					t.Errorf("message %q lacks the status or the allowlisted code", msg)
				}
				if strings.Contains(msg, "https://x.example") {
					t.Error("the message carries error_uri")
				}
			})
		}
	}
}

// echoTransport fails every request with an error that repeats the request it was given.
type echoTransport struct{}

func (echoTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	return nil, fmt.Errorf("dial tcp: echo-marker %s %s %s", r.URL, body, r.Header.Get("Authorization"))
}

func TestNetworkErrorsAreLocallyAuthored(t *testing.T) {
	p := pointedAt("https://provider.invalid/endpoint")
	a := newAuthorization(t, p.OAuth2, "openid")
	want := map[string]string{
		"exchange": "network error contacting the token endpoint",
		"identity": "network error contacting the identity endpoint",
		"revoke":   "network error contacting the revoke endpoint",
	}
	for name, call := range backChannelCalls(p, a) {
		err := call(&http.Client{Transport: echoTransport{}})
		var nerr *oauth.NetworkError
		if !errors.As(err, &nerr) {
			t.Fatalf("%s err = %v, want NetworkError", name, err)
		}
		if err.Error() != want[name] {
			t.Errorf("%s message = %q, want %q", name, err.Error(), want[name])
		}
		if nerr.RequestSent {
			t.Errorf("%s: a request that never reached the wire counts as sent", name)
		}
		assertNoSecretIn(t, err.Error(), a)
	}
}

func TestNetworkErrorKnowsWhetherTheRequestWasSent(t *testing.T) {
	refused := httptest.NewServer(http.NotFoundHandler())
	refused.Close()
	hangUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	}))
	t.Cleanup(hangUp.Close)

	for _, tc := range []struct {
		name string
		url  string
		sent bool
	}{{"refused", refused.URL, false}, {"hung up after reading", hangUp.URL, true}} {
		p := googleLike(tc.url)
		_, err := oauth.Exchange(context.Background(), &http.Client{}, p, client, newAuthorization(t, p.OAuth2), "c")
		var nerr *oauth.NetworkError
		if !errors.As(err, &nerr) || nerr.RequestSent != tc.sent {
			t.Errorf("%s: err = %#v, want NetworkError with RequestSent %v", tc.name, err, tc.sent)
		}
	}
}

func TestBackChannelNeverFollowsRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		var elsewhere atomic.Int32
		second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			elsewhere.Add(1)
			_, _ = io.WriteString(w, standardReply)
		}))
		t.Cleanup(second.Close)
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, second.URL+r.URL.Path, status)
		}))
		t.Cleanup(first.Close)
		p := pointedAt(first.URL)
		a := newAuthorization(t, p.OAuth2, "openid")

		for name, call := range backChannelCalls(p, a) {
			err := call(&http.Client{})
			var perr *oauth.ProviderError
			if !errors.As(err, &perr) || perr.Status != status {
				t.Errorf("%s with %d: err = %v, want ProviderError %d", name, status, err, status)
			}
		}
		if n := elsewhere.Load(); n != 0 {
			t.Errorf("a %d redirect reached the second server %d times", status, n)
		}
	}
}

func TestBackChannelRequiresSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultipleChoices)
		_, _ = io.WriteString(w, standardReply)
	}))
	t.Cleanup(srv.Close)
	p := pointedAt(srv.URL)
	for name, call := range backChannelCalls(p, newAuthorization(t, p.OAuth2)) {
		var perr *oauth.ProviderError
		if err := call(srv.Client()); !errors.As(err, &perr) || perr.Status != 300 {
			t.Errorf("%s: err = %v, want ProviderError 300", name, err)
		}
	}
}
