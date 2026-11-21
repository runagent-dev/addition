package oauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/oauth"
)

// refreshServer records the forms it receives and answers each with reply.
func refreshServer(t *testing.T, reply func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *[]url.Values) {
	t.Helper()
	var forms []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		forms = append(forms, r.PostForm)
		reply(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &forms
}

func refreshReply(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestRefreshSendsTheRefreshGrant(t *testing.T) {
	srv, forms := refreshServer(t, refreshReply(200, `{"access_token":"new-access","expires_in":3600,"token_type":"Bearer","scope":"openid"}`))

	got, err := oauth.Refresh(context.Background(), srv.Client(), googleLike(srv.URL), client, refreshToken)

	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got.AccessToken != "new-access" || got.ExpiresIn != time.Hour || got.RefreshToken != "" {
		t.Errorf("tokens = %+v, want a new access token for an hour and no rotated refresh token", got)
	}
	form := (*forms)[0]
	want := map[string]string{
		"grant_type": "refresh_token", "refresh_token": refreshToken, "client_id": client.ID, "client_secret": clientSecret,
	}
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, form.Get(k), v)
		}
	}
	for _, unwanted := range []string{"code", "code_verifier", "redirect_uri"} {
		if form.Has(unwanted) {
			t.Errorf("the refresh form carries %s", unwanted)
		}
	}
}

func TestRefreshReturnsARotatedRefreshToken(t *testing.T) {
	srv, _ := refreshServer(t, refreshReply(200, `{"access_token":"new-access","refresh_token":"rotated","expires_in":3600}`))

	got, err := oauth.Refresh(context.Background(), srv.Client(), googleLike(srv.URL), client, refreshToken)

	if err != nil || got.RefreshToken != "rotated" {
		t.Errorf("Refresh = %+v, %v; want the rotated refresh token", got, err)
	}
}

func TestRefreshHonoursHeaderClientAuthAndTokenParams(t *testing.T) {
	var user, pass string
	srv, forms := refreshServer(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ = r.BasicAuth()
		refreshReply(200, `{"access_token":"a"}`)(w, r)
	})
	p := googleLike(srv.URL)
	p.OAuth2.ClientAuthMethod = "header"
	p.OAuth2.TokenParams = map[string]string{"audience": "x"}

	if _, err := oauth.Refresh(context.Background(), srv.Client(), p, client, refreshToken); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if user != url.QueryEscape(client.ID) || pass != url.QueryEscape(clientSecret) {
		t.Errorf("basic auth = %q:%q", user, pass)
	}
	form := (*forms)[0]
	if form.Has("client_secret") || form.Get("audience") != "x" {
		t.Errorf("form = %v, want no client_secret and the catalog's token_params", form)
	}
}

func TestRefreshRejectionCarriesOnlyTheOAuthErrorIdentifier(t *testing.T) {
	srv := echoingServer(t, http.StatusBadRequest, "invalid_grant")

	_, err := oauth.Refresh(context.Background(), srv.Client(), googleLike(srv.URL), client, refreshToken)

	var perr *oauth.ProviderError
	if !errors.As(err, &perr) || perr.Op != oauth.OpRefresh || perr.Status != 400 || perr.Code != "invalid_grant" {
		t.Fatalf("err = %#v, want a 400 invalid_grant refresh ProviderError", err)
	}
	for _, secret := range []string{refreshToken, clientSecret, "echo-marker"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error text repeats %q: %s", secret, err)
		}
	}
}

func TestRefreshFailureShapes(t *testing.T) {
	cases := map[string]struct {
		reply  func(http.ResponseWriter, *http.Request)
		status int
	}{
		"server error":         {refreshReply(500, `{"error":"server_error"}`), 500},
		"unknown error code":   {refreshReply(400, `{"error":"`+refreshToken+`"}`), 400},
		"2xx without a token":  {refreshReply(200, `{"expires_in":3600}`), 200},
		"redirect":             {func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "https://elsewhere.example/", 302) }, 302},
		"2xx that is not JSON": {refreshReply(200, `<html>`), 200},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := refreshServer(t, tc.reply)

			_, err := oauth.Refresh(context.Background(), srv.Client(), googleLike(srv.URL), client, refreshToken)

			var perr *oauth.ProviderError
			if !errors.As(err, &perr) || perr.Status != tc.status || perr.Op != oauth.OpRefresh {
				t.Fatalf("err = %#v, want a refresh ProviderError with status %d", err, tc.status)
			}
			if strings.Contains(err.Error(), refreshToken) {
				t.Errorf("the error text repeats the refresh token: %s", err)
			}
		})
	}
}

func TestRefreshNetworkFailure(t *testing.T) {
	srv, _ := refreshServer(t, refreshReply(200, `{}`))
	srv.Close()

	_, err := oauth.Refresh(context.Background(), srv.Client(), googleLike(srv.URL), client, refreshToken)

	var nerr *oauth.NetworkError
	if !errors.As(err, &nerr) || nerr.Op != oauth.OpRefresh {
		t.Fatalf("err = %#v, want a refresh NetworkError", err)
	}
	if strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), refreshToken) {
		t.Errorf("the error text repeats the URL or token: %s", err)
	}
}
