package proxy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/proxy"
	"github.com/runagent-dev/addition/internal/vault"
	"github.com/runagent-dev/addition/shared"
)

type mockResponse struct {
	Status int `yaml:"status"`
	Body   any `yaml:"body"`
}

type refreshScenario struct {
	Name     string `yaml:"name"`
	Provider string `yaml:"provider"`
	Initial  struct {
		AccessToken  string `yaml:"access_token"`
		RefreshToken string `yaml:"refresh_token"`
	} `yaml:"initial"`
	MockResponse mockResponse `yaml:"mock_response"`
	Expected     struct {
		AccessToken  string `yaml:"access_token"`
		RefreshToken string `yaml:"refresh_token"`
		Status       string `yaml:"status"`
	} `yaml:"expected"`
}

type proxyScenario struct {
	Name         string         `yaml:"name"`
	Provider     string         `yaml:"provider"`
	Method       string         `yaml:"method"`
	Path         string         `yaml:"path"`
	Params       map[string]any `yaml:"params"`
	Body         any            `yaml:"body"`
	MockResponse mockResponse   `yaml:"mock_response"`
	Expected     struct {
		Status         int               `yaml:"status"`
		AuthHeader     string            `yaml:"auth_header"`
		AuthPrefix     string            `yaml:"auth_prefix"`
		URLQueryParams map[string]string `yaml:"url_query_params"`
		RequestHasBody bool              `yaml:"request_has_body"`
	} `yaml:"expected"`
}

func loadFixture[T any](t *testing.T, name string) []T {
	t.Helper()
	data, err := os.ReadFile("../../shared/test-fixtures/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Scenarios []T `yaml:"scenarios"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(doc.Scenarios) == 0 {
		t.Fatalf("%s has no scenarios", name)
	}
	return doc.Scenarios
}

// The fixture's status words are the SDK's; the CLI stores a dead refresh token as needs-reauth.
var fixtureStatus = map[string]string{"active": vault.AccountActive, "refresh_failed": vault.AccountNeedsReauth}

func TestTokenRefreshFixtures(t *testing.T) {
	for _, sc := range loadFixture[refreshScenario](t, "token_refresh.yaml") {
		t.Run(sc.Name, func(t *testing.T) {
			f := newVaultFixture(t, vault.Credential{
				AccessToken: sc.Initial.AccessToken, RefreshToken: sc.Initial.RefreshToken, ExpiresAt: t0.Add(-time.Minute),
			})
			f.endpoint.reply = func(int, url.Values) (int, map[string]any) {
				body, _ := sc.MockResponse.Body.(map[string]any)
				return sc.MockResponse.Status, body
			}
			if sc.Provider != "google" {
				t.Fatalf("fixture names provider %q; the driver seeds google", sc.Provider)
			}

			_, _ = f.manager(f.store).Access(context.Background())

			cred := f.credential()
			if cred.AccessToken != sc.Expected.AccessToken || cred.RefreshToken != sc.Expected.RefreshToken {
				t.Errorf("tokens = %q/%q, want %q/%q", cred.AccessToken, cred.RefreshToken, sc.Expected.AccessToken, sc.Expected.RefreshToken)
			}
			if want := fixtureStatus[sc.Expected.Status]; f.status() != want {
				t.Errorf("status = %q, want %q (fixture %q)", f.status(), want, sc.Expected.Status)
			}
		})
	}
}

func TestProxyRequestFixtures(t *testing.T) {
	c, err := catalog.Load(shared.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range loadFixture[proxyScenario](t, "proxy_requests.yaml") {
		t.Run(sc.Name, func(t *testing.T) {
			p, ok := c.Provider(sc.Provider)
			if !ok {
				t.Fatalf("fixture names unknown provider %q", sc.Provider)
			}
			up := newUpstream(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(sc.MockResponse.Status)
				_ = json.NewEncoder(w).Encode(sc.MockResponse.Body)
			})
			var flags []string
			for k, v := range sc.Params {
				flags = append(flags, fmt.Sprintf("%s=%v", k, v))
			}
			target, err := proxy.ResolveURL(p.API, sc.Path, flags)
			if err != nil {
				t.Fatalf("ResolveURL: %v", err)
			}
			if target.Host != mustHost(t, p.API.BaseURL) {
				t.Errorf("host = %s, want the provider's base host", target.Host)
			}
			target.Scheme, target.Host = "http", up.srv.Listener.Addr().String()
			req := proxy.Request{Method: sc.Method, URL: target}
			if sc.Body != nil {
				req.Body, _ = json.Marshal(sc.Body)
			}
			e := newEngine(&fakeRenewer{}, &sleeps{})
			e.API = p.API

			resp, _, err := do(t, e, req, "tok")

			if err != nil || resp.Status != sc.Expected.Status {
				t.Fatalf("Do = %v, %v; want status %d", resp, err, sc.Expected.Status)
			}
			got := up.seen()[0]
			header, prefix := p.API.AuthHeader, p.API.AuthPrefix
			if sc.Expected.AuthHeader != "" {
				header, prefix = sc.Expected.AuthHeader, sc.Expected.AuthPrefix
			}
			if want := prefix + " tok"; got.header.Get(header) != want {
				t.Errorf("%s = %q, want %q", header, got.header.Get(header), want)
			}
			query, _ := url.ParseQuery(got.rawQuery)
			for k, v := range sc.Expected.URLQueryParams {
				if query.Get(k) != v {
					t.Errorf("query %s = %q, want %q", k, query.Get(k), v)
				}
			}
			if hasBody := got.body != ""; hasBody != sc.Expected.RequestHasBody {
				t.Errorf("request has body = %v, want %v", hasBody, sc.Expected.RequestHasBody)
			}
			if sc.Expected.RequestHasBody && got.contentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got.contentType)
			}
			if !slices.Contains([]string{"GET", "POST"}, got.method) || got.method != sc.Method {
				t.Errorf("method = %s, want %s", got.method, sc.Method)
			}
		})
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}
