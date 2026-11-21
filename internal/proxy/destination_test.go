package proxy_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/proxy"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestCheckDestinationAllowsOnlyListedHostsOverHTTPS(t *testing.T) {
	api := catalog.API{BaseURL: "https://api.example.com", AllowedHosts: []string{"api.example.com", "upload.example.com"}}
	cases := []struct {
		url   string
		allow bool
	}{
		{"https://api.example.com/v1/x", true},
		{"https://UPLOAD.example.com./v1/x", true},
		{"https://api.example.com:443/v1/x", true},
		{"https://evil.example/v1/x", false},
		{"https://api.example.com.evil.example/", false},
		{"https://api.example.com:8443/v1/x", false},
		{"http://api.example.com/v1/x", false},
		{"https://user:pw@api.example.com/v1/x", false},
		{"ftp://api.example.com/", false},
		{"https:///no-host", false},
	}
	for _, tc := range cases {
		err := proxy.CheckDestination(api, mustParseURL(t, tc.url), false)

		if tc.allow != (err == nil) {
			t.Errorf("%s: err = %v, want allowed = %v", tc.url, err, tc.allow)
		}
		if err != nil && requestErrorCode(err) != "host-not-allowed" {
			t.Errorf("%s: err = %v, want code host-not-allowed", tc.url, err)
		}
	}
}

func TestCheckDestinationHonoursTheCleartextTestSeam(t *testing.T) {
	api := catalog.API{AllowedHosts: []string{"127.0.0.1:9999"}}

	if err := proxy.CheckDestination(api, mustParseURL(t, "http://127.0.0.1:9999/x"), true); err != nil {
		t.Errorf("seam on: %v", err)
	}
	if err := proxy.CheckDestination(api, mustParseURL(t, "http://127.0.0.1:9999/x"), false); err == nil {
		t.Error("seam off: a cleartext URL was allowed")
	}
	if err := proxy.CheckDestination(api, mustParseURL(t, "http://127.0.0.1:1/x"), true); err == nil {
		t.Error("seam on: an unlisted host was allowed")
	}
}

func TestEngineNeverAttachesATokenOutsideTheAllowList(t *testing.T) {
	up := newUpstream(t, reply(200, "stolen"))
	cases := map[string]func(*proxy.Engine){
		"unlisted host":    func(e *proxy.Engine) { e.API.AllowedHosts = []string{"api.example.com"} },
		"cleartext scheme": func(e *proxy.Engine) { e.AllowCleartext = false },
	}
	for name, configure := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEngine(&fakeRenewer{}, &sleeps{})
			transport := &countingTransport{base: http.DefaultTransport}
			e.Client = &http.Client{Transport: transport}
			configure(e)

			_, err := e.Do(context.Background(), proxy.Request{Method: "GET", URL: up.url("/")}, oldAccess)

			if requestErrorCode(err) != "host-not-allowed" {
				t.Fatalf("err = %v, want code host-not-allowed", err)
			}
			if transport.calls.Load() != 0 || len(up.seen()) != 0 {
				t.Errorf("transport calls %d, server requests %d; nothing may be sent", transport.calls.Load(), len(up.seen()))
			}
		})
	}
}
