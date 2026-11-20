package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/oauth"
)

const (
	clientSecret = "GOCSPX-oauth-test-secret"
	accessToken  = "ya29.oauth-test-access"
	refreshToken = "1//oauth-test-refresh"
)

var client = oauth.Client{ID: "client-id.apps.example", Secret: clientSecret}

func googleLike(tokenURL string) *catalog.Provider {
	return &catalog.Provider{
		ID: "google",
		OAuth2: &catalog.OAuth2{
			AuthorizeURL:        "https://auth.example/authorize",
			TokenURL:            tokenURL,
			RevokeURL:           tokenURL + "/revoke",
			PKCESupported:       true,
			ScopesDelimiter:     "space",
			AuthorizeParams:     map[string]string{"access_type": "offline", "prompt": "consent"},
			TokenParams:         map[string]string{},
			TokenResponseFormat: "json",
			ClientAuthMethod:    "body",
			Identity:            &catalog.Identity{URL: tokenURL + "/userinfo", Field: "email", SubjectField: "sub"},
		},
		API: catalog.API{AuthHeader: "Authorization", AuthPrefix: "Bearer"},
	}
}

func newAuthorization(t *testing.T, cfg *catalog.OAuth2, scopes ...string) oauth.Authorization {
	t.Helper()
	a, err := oauth.NewAuthorization(cfg, "http://127.0.0.1:5555/callback", scopes)
	if err != nil {
		t.Fatalf("NewAuthorization: %v", err)
	}
	return a
}

func TestAuthorizeURLCarriesCatalogData(t *testing.T) {
	p := googleLike("https://token.example")
	a := newAuthorization(t, p.OAuth2, "openid", "https://scope.example/drive")

	u, err := url.Parse(a.URL(p.OAuth2, client))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	want := map[string]string{
		"response_type":         "code",
		"client_id":             client.ID,
		"redirect_uri":          "http://127.0.0.1:5555/callback",
		"state":                 a.State,
		"scope":                 "openid https://scope.example/drive",
		"access_type":           "offline",
		"prompt":                "consent",
		"code_challenge":        oauth.Challenge(a.Verifier),
		"code_challenge_method": "S256",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if u.Scheme+"://"+u.Host+u.Path != "https://auth.example/authorize" {
		t.Errorf("authorize endpoint = %s", u)
	}
	if strings.Contains(u.String(), a.Verifier) || strings.Contains(u.String(), clientSecret) {
		t.Error("the authorize URL carries the verifier or the client secret")
	}
}

func TestChallengeIsS256OfTheVerifier(t *testing.T) {
	sum := sha256.Sum256([]byte("verifier"))
	if got, want := oauth.Challenge("verifier"), base64.RawURLEncoding.EncodeToString(sum[:]); got != want {
		t.Errorf("Challenge = %q, want %q", got, want)
	}
}

func TestAuthorizationsAreRandom(t *testing.T) {
	p := googleLike("https://token.example")
	a, b := newAuthorization(t, p.OAuth2), newAuthorization(t, p.OAuth2)
	if a.State == b.State || a.Verifier == b.Verifier {
		t.Error("two authorizations share a state or verifier")
	}
	if len(a.State) < 32 {
		t.Errorf("state %q is too short", a.State)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`).MatchString(a.Verifier) {
		t.Errorf("verifier %q is not RFC 7636 shaped", a.Verifier)
	}
}

func TestNoPKCEWhenUnsupported(t *testing.T) {
	p := googleLike("https://token.example")
	p.OAuth2.PKCESupported = false
	p.OAuth2.ScopesDelimiter = "comma"
	a := newAuthorization(t, p.OAuth2, "a:read", "b:write")

	if a.Verifier != "" {
		t.Errorf("verifier = %q, want none", a.Verifier)
	}
	q := mustQuery(t, a.URL(p.OAuth2, client))
	if q.Has("code_challenge") || q.Has("code_challenge_method") {
		t.Errorf("PKCE params sent: %v", q)
	}
	if q.Get("scope") != "a:read,b:write" {
		t.Errorf("scope = %q, want comma-joined", q.Get("scope"))
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestParseCallback(t *testing.T) {
	cb, err := oauth.ParseCallback("http://127.0.0.1:5555/callback?code=the-code&state=s1&scope=openid")
	if err != nil {
		t.Fatal(err)
	}
	if cb.Code != "the-code" || cb.State != "s1" || cb.RedirectURI != "http://127.0.0.1:5555/callback" {
		t.Errorf("callback = %+v", cb)
	}

	for _, bad := range []string{"", "not a url", "http://127.0.0.1:5555/callback", "code=x&state=y", "http://127.0.0.1/callback?code=x",
		"http://127.0.0.1/callback?code=x&error=access_denied&state=y",
		"http://127.0.0.1/callback?code=&error=access_denied&state=y",
		"http://127.0.0.1/callback?error=access_denied&error=server_error&state=y",
		"http://127.0.0.1/callback?code=a&code=b&state=y",
		"http://127.0.0.1/callback?code=x&state=y&state=z"} {
		if _, err := oauth.ParseCallback(bad); !errors.Is(err, oauth.ErrBadRedirect) {
			t.Errorf("ParseCallback(%q) err = %v, want ErrBadRedirect", bad, err)
		}
	}
}

func TestCallbackVerify(t *testing.T) {
	ok := oauth.Callback{Code: "c", State: "s"}
	if code, err := ok.Verify("s"); err != nil || code != "c" {
		t.Errorf("Verify = %q, %v", code, err)
	}
	if _, err := ok.Verify("other"); !errors.Is(err, oauth.ErrStateMismatch) {
		t.Errorf("mismatch err = %v, want ErrStateMismatch", err)
	}

	denied := oauth.Callback{State: "s", Error: "access_denied"}
	_, err := denied.Verify("s")
	var authErr *oauth.AuthorizationError
	if !errors.As(err, &authErr) || authErr.Code != "access_denied" || !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("denied err = %v, want AuthorizationError access_denied", err)
	}
	odd := oauth.Callback{State: "s", Error: "echo-marker " + clientSecret}
	if _, err := odd.Verify("s"); !errors.As(err, &authErr) || authErr.Code != "" || strings.Contains(err.Error(), "echo-marker") {
		t.Errorf("an unlisted callback error = %v, want an AuthorizationError without its code", err)
	}
	if _, err := (oauth.Callback{State: "s"}).Verify("s"); !errors.Is(err, oauth.ErrBadRedirect) {
		t.Errorf("missing code err = %v, want ErrBadRedirect", err)
	}
	if _, err := (oauth.Callback{State: "s", Code: "c", Error: "access_denied"}).Verify("s"); !errors.Is(err, oauth.ErrBadRedirect) {
		t.Errorf("a code with an error = %v, want ErrBadRedirect", err)
	}
}

// tokenServer records the last token request and answers with respond.
type tokenServer struct {
	*httptest.Server
	form    url.Values
	header  http.Header
	respond func(w http.ResponseWriter)
}

func newTokenServer(t *testing.T, respond func(w http.ResponseWriter)) *tokenServer {
	ts := &tokenServer{respond: respond}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		ts.form, ts.header = r.PostForm, r.Header.Clone()
		ts.respond(w)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func jsonReply(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

const standardReply = `{"access_token":"` + accessToken + `","refresh_token":"` + refreshToken + `","token_type":"Bearer","expires_in":3599,"scope":"openid https://scope.example/drive"}`

func TestExchangeWithBodyClientAuth(t *testing.T) {
	ts := newTokenServer(t, jsonReply(200, standardReply))
	p := googleLike(ts.URL)
	p.OAuth2.TokenParams = map[string]string{"extra": "1"}
	a := newAuthorization(t, p.OAuth2, "openid")

	tokens, err := oauth.Exchange(context.Background(), ts.Client(), p, client, a, "the-code")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	want := map[string]string{
		"grant_type": "authorization_code", "code": "the-code", "redirect_uri": a.RedirectURI,
		"client_id": client.ID, "client_secret": clientSecret, "code_verifier": a.Verifier, "extra": "1",
	}
	for k, v := range want {
		if ts.form.Get(k) != v {
			t.Errorf("form %s = %q, want %q", k, ts.form.Get(k), v)
		}
	}
	if ts.header.Get("Authorization") != "" {
		t.Error("body auth also sent an Authorization header")
	}
	if tokens.AccessToken != accessToken || tokens.RefreshToken != refreshToken || tokens.TokenType != "Bearer" {
		t.Errorf("tokens = %+v", tokens)
	}
	if tokens.ExpiresIn != 3599*time.Second {
		t.Errorf("expires_in = %v", tokens.ExpiresIn)
	}
	if !slices.Equal(tokens.Scopes, []string{"openid", "https://scope.example/drive"}) {
		t.Errorf("scopes = %v", tokens.Scopes)
	}
}

func TestExchangeWithHeaderClientAuth(t *testing.T) {
	ts := newTokenServer(t, jsonReply(200, `{"access_token":"`+accessToken+`"}`))
	p := googleLike(ts.URL)
	p.OAuth2.ClientAuthMethod = "header"
	a := newAuthorization(t, p.OAuth2, "openid")

	tokens, err := oauth.Exchange(context.Background(), ts.Client(), p, client, a, "c")
	if err != nil {
		t.Fatal(err)
	}
	user, pass, ok := (&http.Request{Header: ts.header}).BasicAuth()
	if !ok || user != client.ID || pass != clientSecret {
		t.Errorf("basic auth = %q/%q/%v", user, pass, ok)
	}
	if ts.form.Has("client_secret") {
		t.Error("header auth also sent client_secret in the body")
	}
	if tokens.RefreshToken != "" || tokens.ExpiresIn != 0 || tokens.Scopes != nil {
		t.Errorf("absent fields came back set: %+v", tokens)
	}
}

func TestExchangeHonoursTokenResponseMapping(t *testing.T) {
	ts := newTokenServer(t, jsonReply(200, `{"ok":true,"authed_user":{"access_token":"`+accessToken+`","refresh_token":"`+refreshToken+`","expires_in":"600"}}`))
	p := googleLike(ts.URL)
	p.OAuth2.TokenResponseMapping = &catalog.TokenResponseMapping{
		AccessToken: "authed_user.access_token", RefreshToken: "authed_user.refresh_token", ExpiresIn: "authed_user.expires_in",
	}

	tokens, err := oauth.Exchange(context.Background(), ts.Client(), p, client, newAuthorization(t, p.OAuth2), "c")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != accessToken || tokens.RefreshToken != refreshToken || tokens.ExpiresIn != 600*time.Second {
		t.Errorf("tokens = %+v", tokens)
	}
}

func TestExchangeParsesFormResponses(t *testing.T) {
	ts := newTokenServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
		_, _ = io.WriteString(w, "access_token="+accessToken+"&scope=a%2Cb&token_type=bearer")
	})
	p := googleLike(ts.URL)
	p.OAuth2.TokenResponseFormat = "form"
	p.OAuth2.ScopesDelimiter = "comma"

	tokens, err := oauth.Exchange(context.Background(), ts.Client(), p, client, newAuthorization(t, p.OAuth2), "c")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != accessToken || !slices.Equal(tokens.Scopes, []string{"a", "b"}) {
		t.Errorf("tokens = %+v", tokens)
	}
}

func TestExchangeProviderRejection(t *testing.T) {
	ts := newTokenServer(t, jsonReply(400, `{"error":"invalid_grant","error_description":"Bad Request"}`))
	p := googleLike(ts.URL)

	_, err := oauth.Exchange(context.Background(), ts.Client(), p, client, newAuthorization(t, p.OAuth2), "c")
	var perr *oauth.ProviderError
	if !errors.As(err, &perr) || perr.Status != 400 || perr.Code != "invalid_grant" {
		t.Fatalf("err = %v, want ProviderError 400 invalid_grant", err)
	}
	if strings.Contains(err.Error(), clientSecret) {
		t.Error("the error carries the client secret")
	}
}

func TestExchangeRejectsAResponseWithoutAnAccessToken(t *testing.T) {
	ts := newTokenServer(t, jsonReply(200, `{"token_type":"Bearer"}`))
	p := googleLike(ts.URL)

	_, err := oauth.Exchange(context.Background(), ts.Client(), p, client, newAuthorization(t, p.OAuth2), "c")
	var perr *oauth.ProviderError
	if !errors.As(err, &perr) || perr.Status != 200 || perr.Code != "" || perr.Problem == "" {
		t.Errorf("err = %#v, want a malformed-response ProviderError", err)
	}
}

func TestExchangeNetworkFailure(t *testing.T) {
	ts := newTokenServer(t, jsonReply(200, standardReply))
	ts.Close()
	p := googleLike(ts.URL)

	_, err := oauth.Exchange(context.Background(), http.DefaultClient, p, client, newAuthorization(t, p.OAuth2), "c")
	var nerr *oauth.NetworkError
	if !errors.As(err, &nerr) {
		t.Errorf("err = %v, want NetworkError", err)
	}
}

func TestFetchIdentity(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if auth != "Bearer "+accessToken {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, `{"profile":{"email":"me@work.example","id":"1001"},"numeric":42}`)
	}))
	defer srv.Close()
	p := googleLike(srv.URL)
	p.OAuth2.Identity = &catalog.Identity{URL: srv.URL, Field: "profile.email", SubjectField: "profile.id"}

	got, err := oauth.FetchIdentity(context.Background(), srv.Client(), p, accessToken)
	if err != nil || got != (oauth.Identity{Subject: "1001", Display: "me@work.example"}) {
		t.Fatalf("FetchIdentity = %+v, %v", got, err)
	}

	p.OAuth2.Identity.SubjectField = "numeric"
	if got, err := oauth.FetchIdentity(context.Background(), srv.Client(), p, accessToken); err != nil || got.Subject != "42" {
		t.Errorf("a numeric subject = %+v, %v; want 42", got, err)
	}

	_, err = oauth.FetchIdentity(context.Background(), srv.Client(), p, "wrong-token")
	var perr *oauth.ProviderError
	if !errors.As(err, &perr) || perr.Status != 401 {
		t.Errorf("err = %v, want ProviderError 401", err)
	}

	for _, field := range []string{"Field", "SubjectField"} {
		id := catalog.Identity{URL: srv.URL, Field: "profile.email", SubjectField: "profile.id"}
		if field == "Field" {
			id.Field = "profile.missing"
		} else {
			id.SubjectField = "profile.missing"
		}
		p.OAuth2.Identity = &id
		if _, err := oauth.FetchIdentity(context.Background(), srv.Client(), p, accessToken); !errors.As(err, &perr) || perr.Status != 200 {
			t.Errorf("a missing %s = %v, want a malformed-response ProviderError", field, err)
		}
	}

	p.OAuth2.Identity = nil
	if got, err := oauth.FetchIdentity(context.Background(), srv.Client(), p, accessToken); err != nil || got != (oauth.Identity{}) {
		t.Errorf("no identity config = %+v, %v; want empty", got, err)
	}
}

func TestExchangeScopeField(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  []string
		bad   bool
	}{
		{"absent means the requested scopes", `{"access_token":"a"}`, nil, false},
		{"empty means none granted", `{"access_token":"a","scope":""}`, []string{}, false},
		{"listed", `{"access_token":"a","scope":"openid email"}`, []string{"openid", "email"}, false},
		{"not a string", `{"access_token":"a","scope":["openid"]}`, nil, true},
		{"null", `{"access_token":"a","scope":null}`, nil, true},
		{"a number", `{"access_token":"a","scope":7}`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTokenServer(t, jsonReply(200, tc.reply))
			p := googleLike(ts.URL)
			tokens, err := oauth.Exchange(context.Background(), ts.Client(), p, client, newAuthorization(t, p.OAuth2), "c")
			if tc.bad {
				var perr *oauth.ProviderError
				if !errors.As(err, &perr) || perr.Status != 200 {
					t.Errorf("err = %v, want a malformed-response ProviderError", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (tokens.Scopes == nil) != (tc.want == nil) || !slices.Equal(tokens.Scopes, tc.want) {
				t.Errorf("scopes = %#v, want %#v", tokens.Scopes, tc.want)
			}
		})
	}
}

func TestRevoke(t *testing.T) {
	ts := newTokenServer(t, jsonReply(200, `{}`))
	p := googleLike(ts.URL)

	if err := oauth.Revoke(context.Background(), ts.Client(), p, refreshToken); err != nil {
		t.Fatal(err)
	}
	if ts.form.Get("token") != refreshToken {
		t.Errorf("revoked token = %q", ts.form.Get("token"))
	}

	ts.respond = jsonReply(400, `{"error":"invalid_token"}`)
	var perr *oauth.ProviderError
	if err := oauth.Revoke(context.Background(), ts.Client(), p, refreshToken); !errors.As(err, &perr) {
		t.Errorf("err = %v, want ProviderError", err)
	} else if strings.Contains(err.Error(), refreshToken) {
		t.Error("the revoke error carries the token")
	}

	p.OAuth2.RevokeURL = ""
	if err := oauth.Revoke(context.Background(), ts.Client(), p, refreshToken); !errors.Is(err, oauth.ErrRevokeUnsupported) {
		t.Errorf("err = %v, want ErrRevokeUnsupported", err)
	}
}

func listen(t *testing.T, state string) *oauth.Loopback {
	t.Helper()
	lb, err := oauth.ListenLoopback(0, state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lb.Close() })
	return lb
}

func get(t *testing.T, u string) (int, string) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(page)
}

func received(lb *oauth.Loopback) (oauth.Callback, bool) {
	select {
	case cb := <-lb.Received():
		return cb, true
	case <-time.After(50 * time.Millisecond):
		return oauth.Callback{}, false
	}
}

func TestLoopbackAcceptsOnlyTheVerifiedCallback(t *testing.T) {
	lb := listen(t, "s1")
	if !regexp.MustCompile(`^http://127\.0\.0\.1:\d+/callback$`).MatchString(lb.RedirectURI()) {
		t.Fatalf("redirect URI = %q", lb.RedirectURI())
	}
	if lb.LastRejection() != nil {
		t.Errorf("a fresh listener has a rejection: %v", lb.LastRejection())
	}

	base := strings.TrimSuffix(lb.RedirectURI(), "/callback")
	if status, _ := get(t, base+"/favicon.ico"); status != 404 {
		t.Errorf("other path: %d", status)
	}
	if status, _ := get(t, lb.RedirectURI()); status != 400 {
		t.Errorf("a callback without state: %d", status)
	}
	if status, _ := get(t, lb.RedirectURI()+"?code=forged&state=wrong"); status != 400 {
		t.Errorf("a wrong state: %d", status)
	}
	if !errors.Is(lb.LastRejection(), oauth.ErrStateMismatch) {
		t.Errorf("last rejection = %v, want ErrStateMismatch", lb.LastRejection())
	}
	if status, _ := get(t, lb.RedirectURI()+"?state=s1"); status != 400 {
		t.Errorf("state without code or error: %d", status)
	}
	if !errors.Is(lb.LastRejection(), oauth.ErrBadRedirect) {
		t.Errorf("last rejection = %v, want ErrBadRedirect", lb.LastRejection())
	}
	if status, _ := get(t, lb.RedirectURI()+"?code=c0&error=access_denied&state=s1"); status != 400 {
		t.Errorf("a code with an error: %d", status)
	}
	if _, ok := received(lb); ok {
		t.Fatal("an invalid callback was delivered")
	}

	status, page := get(t, lb.RedirectURI()+"?code=c1&state=s1")
	if status != 200 || !strings.Contains(page, "close this tab") {
		t.Errorf("valid callback: %d %q", status, page)
	}
	if status, _ := get(t, lb.RedirectURI()+"?code=c2&state=s1"); status == 200 {
		t.Error("a second valid callback was accepted")
	}
	cb, ok := received(lb)
	if !ok || cb.Code != "c1" || cb.State != "s1" || cb.RedirectURI != lb.RedirectURI() {
		t.Errorf("received = %+v, %v", cb, ok)
	}
	if _, ok := received(lb); ok {
		t.Error("a second callback was delivered")
	}
}

func TestLoopbackAcceptsAVerifiedDenial(t *testing.T) {
	lb := listen(t, "s1")
	if status, _ := get(t, lb.RedirectURI()+"?error=access_denied&state=s1"); status != 200 {
		t.Errorf("a verified denial got %d", status)
	}
	cb, ok := received(lb)
	if !ok || cb.Error != "access_denied" {
		t.Errorf("received = %+v, %v", cb, ok)
	}
}

func TestLoopbackStopsListeningWhenClosed(t *testing.T) {
	lb := listen(t, "s1")
	uri := lb.RedirectURI()
	if err := lb.Close(); err != nil {
		t.Fatal(err)
	}
	if resp, err := http.Get(uri + "?code=c&state=s1"); err == nil {
		_ = resp.Body.Close()
		t.Error("the listener still answers after Close")
	}
}
