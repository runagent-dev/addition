package proxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/proxy"
	"github.com/runagent-dev/addition/internal/vault"
	"github.com/runagent-dev/addition/shared"
)

const (
	clientID     = "1234-proxy.apps.example"
	clientSecret = "GOCSPX-proxy-test-secret"
	oldAccess    = "ya29.proxy-old-access"
	oldRefresh   = "1//proxy-old-refresh"
	newAccess    = "ya29.proxy-new-access"
	newRefresh   = "1//proxy-new-refresh"
)

var t0 = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// tokenEndpoint is a fake OAuth token endpoint that counts refresh grants.
type tokenEndpoint struct {
	srv   *httptest.Server
	hits  atomic.Int32
	mu    sync.Mutex
	forms []url.Values
	reply func(n int, form url.Values) (status int, body map[string]any)
}

func newTokenEndpoint(t *testing.T) *tokenEndpoint {
	t.Helper()
	e := &tokenEndpoint{reply: func(int, url.Values) (int, map[string]any) {
		return 200, map[string]any{"access_token": newAccess, "expires_in": 3600, "token_type": "Bearer"}
	}}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		n := int(e.hits.Add(1))
		e.mu.Lock()
		e.forms = append(e.forms, r.PostForm)
		e.mu.Unlock()
		status, body := e.reply(n, r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *tokenEndpoint) form(i int) url.Values {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.forms[i]
}

// gmailProvider is the shipped gmail app, which inherits Google's token endpoint, pointed at tokenURL.
func gmailProvider(t *testing.T, tokenURL string) *catalog.Provider {
	t.Helper()
	c, err := catalog.Load(shared.FS)
	if err != nil {
		t.Fatal(err)
	}
	shipped, _ := c.Provider("gmail")
	p := *shipped
	oauth2 := *shipped.OAuth2
	oauth2.TokenURL = tokenURL
	p.OAuth2 = &oauth2
	return &p
}

// vaultFixture is an initialised vault holding one gmail account, "work",, with a credential.
type vaultFixture struct {
	t        *testing.T
	env      vault.Env
	clock    *fakeClock
	store    *vault.Store
	account  vault.Account
	provider *catalog.Provider
	endpoint *tokenEndpoint
}

func newVaultFixture(t *testing.T, cred vault.Credential) *vaultFixture {
	t.Helper()
	keyring.MockInit()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADDN_HOME", filepath.Join(dir, "addn"))
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", home)
	_ = os.Unsetenv("ADDN_MASTER_KEY")
	clock := &fakeClock{t: t0}
	env := vault.OSEnv()
	env.Now = clock.Now
	ctx := context.Background()
	if _, err := vault.Init(ctx, env, vault.InitOptions{KeyFile: true}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	f := &vaultFixture{t: t, env: env, clock: clock, endpoint: newTokenEndpoint(t)}
	f.provider = gmailProvider(t, f.endpoint.srv.URL)
	f.store = f.open()
	acct, err := f.store.CreateAccount(ctx, vault.Account{Provider: "gmail", Alias: "work", Identity: "me@work.example", Scopes: []string{"openid"}})
	if err != nil {
		t.Fatal(err)
	}
	f.account = acct
	if err := f.store.PutOAuthClient(ctx, vault.OAuthClient{Provider: "google", ClientID: clientID, ClientSecret: clientSecret}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutCredential(ctx, acct.ID, cred); err != nil {
		t.Fatal(err)
	}
	return f
}

// open returns another connection to the vault, as a second addn process would have.
func (f *vaultFixture) open() *vault.Store {
	f.t.Helper()
	s, err := vault.Open(context.Background(), f.env)
	if err != nil {
		f.t.Fatalf("Open: %v", err)
	}
	f.t.Cleanup(func() { _ = s.Close() })
	return s
}

func (f *vaultFixture) manager(s *vault.Store) *proxy.TokenManager {
	return &proxy.TokenManager{Store: s, Client: f.endpoint.srv.Client(), Provider: f.provider, Account: f.account, Now: f.clock.Now}
}

func (f *vaultFixture) credential() vault.Credential {
	f.t.Helper()
	cred, err := f.store.GetCredential(context.Background(), f.account.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return cred
}

func (f *vaultFixture) status() string {
	f.t.Helper()
	acct, err := f.store.GetAccount(context.Background(), "gmail", "work")
	if err != nil {
		f.t.Fatal(err)
	}
	return acct.Status
}

func (f *vaultFixture) auditDetails(event string) []string {
	f.t.Helper()
	db := f.rawDB()
	rows, err := db.Query(`SELECT details FROM audit_log WHERE event = ? ORDER BY id`, event)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		_ = rows.Scan(&d)
		out = append(out, d)
	}
	return out
}
