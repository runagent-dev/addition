package proxy_test

import (
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/proxy"
)

var googleAPI = catalog.API{
	BaseURL:      "https://www.googleapis.com",
	AllowedHosts: []string{"www.googleapis.com", "gmail.googleapis.com", "docs.googleapis.com", "127.0.0.1:8443"},
}

func requestErrorCode(err error) string {
	var rerr *proxy.RequestError
	if errors.As(err, &rerr) {
		return rerr.Code
	}
	return ""
}

func TestResolveURLAcceptsPathsAndAllowedHosts(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		query []string
		want  string
	}{
		{"relative path", "/calendar/v3/calendars/primary/events", nil, "https://www.googleapis.com/calendar/v3/calendars/primary/events"},
		{"relative root", "/", nil, "https://www.googleapis.com/"},
		{"path keeps its own query", "/drive/v3/files?pageSize=5", nil, "https://www.googleapis.com/drive/v3/files?pageSize=5"},
		{"allowed absolute host", "https://gmail.googleapis.com/gmail/v1/users/me/profile", nil, "https://gmail.googleapis.com/gmail/v1/users/me/profile"},
		{"host is case-insensitive", "https://GMAIL.GoogleAPIs.com/x", nil, "https://gmail.googleapis.com/x"},
		{"scheme is case-insensitive", "HTTPS://gmail.googleapis.com/x", nil, "https://gmail.googleapis.com/x"},
		{"trailing dot is the same host", "https://gmail.googleapis.com./x", nil, "https://gmail.googleapis.com/x"},
		{"explicit 443 is the same host", "https://gmail.googleapis.com:443/x", nil, "https://gmail.googleapis.com/x"},
		{"listed host with a port", "https://127.0.0.1:8443/x", nil, "https://127.0.0.1:8443/x"},
		{"fragment is never sent", "/x#frag", nil, "https://www.googleapis.com/x"},
		{"encoded path survives", "/drive/v3/files/a%2Fb", nil, "https://www.googleapis.com/drive/v3/files/a%2Fb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := proxy.ResolveURL(googleAPI, tc.path, tc.query)

			if err != nil {
				t.Fatalf("ResolveURL: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("URL = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestResolveURLMergesQueryFlagsAfterThePathsQuery(t *testing.T) {
	got, err := proxy.ResolveURL(googleAPI, "/drive/v3/files?pageSize=5&q=a", []string{"q=name contains 'x'", "fields=files(id,name)", "empty="})

	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{"pageSize": {"5"}, "q": {"a", "name contains 'x'"}, "fields": {"files(id,name)"}, "empty": {""}}
	if q := got.Query(); !queryEqual(q, want) {
		t.Errorf("query = %v, want %v", q, want)
	}
	if got.RawQuery[:len("pageSize=5&q=a&")] != "pageSize=5&q=a&" {
		t.Errorf("the path's own query must come first: %s", got.RawQuery)
	}
}

func queryEqual(a, b url.Values) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range b {
		if !slices.Equal(a[k], v) {
			return false
		}
	}
	return true
}

func TestResolveURLRefusesHostsOutsideTheAllowList(t *testing.T) {
	for _, target := range []string{
		"https://evil.example/x",
		"http://www.googleapis.com/x",
		"https://www.googleapis.com.evil.example/x",
		"https://evil.example/www.googleapis.com",
		"https://evil.example#@www.googleapis.com/",
		"https://evil.example?www.googleapis.com/",
		"https://evil.example\\@www.googleapis.com/",
		"https://user@www.googleapis.com/x",
		"https://user:pw@www.googleapis.com/x",
		"https://www.googleapis.com@evil.example/x",
		"https://www.googleapis.com:8443/x",
		"https://www.googleapis.com:444/x",
		"https://xn--www.googleapis.com/x",
		"https://www.googleapis.com%2eevil.example/x",
		"https://www.googleapis.com%00.evil.example/x",
		"https://evil.example./x",
		"https:///x",
		"https://",
		"https:www.googleapis.com/x",
		"ftp://www.googleapis.com/x",
		"//evil.example/x",
		"//www.googleapis.com/x",
		"///evil.example/x",
		"javascript:alert(1)",
		"evil.example:443/x",
		"mailto:me@www.googleapis.com",
		"https://127.0.0.1:9999/x",
	} {
		t.Run(target, func(t *testing.T) {
			got, err := proxy.ResolveURL(googleAPI, target, nil)

			if err == nil {
				t.Fatalf("ResolveURL accepted %q as %s", target, got)
			}
			if code := requestErrorCode(err); code != "host-not-allowed" {
				t.Errorf("code = %q (%v), want host-not-allowed", code, err)
			}
		})
	}
}

func TestResolveURLRefusesMalformedRelativeInput(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"empty", ""},
		{"no leading slash", "drive/v3/files"},
		{"bare host", "www.googleapis.com/drive"},
		{"space", "/drive/v3/my files"},
		{"control character", "/x\x01y"},
		{"newline", "/x\ny"},
		{"bad escape", "/x%zz"},
		{"bad query escape", "/x?a=%zz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := proxy.ResolveURL(googleAPI, tc.path, nil); requestErrorCode(err) != "bad-path" {
				t.Errorf("err = %v, want code bad-path", err)
			}
		})
	}
}

func TestResolveURLRefusesMalformedQueryFlags(t *testing.T) {
	for _, flag := range []string{"novalue", "=v", ""} {
		if _, err := proxy.ResolveURL(googleAPI, "/x", []string{flag}); requestErrorCode(err) != "bad-query" {
			t.Errorf("--query %q: err = %v, want code bad-query", flag, err)
		}
	}
}

func TestResolveURLErrorsNameNeitherTheQueryNorUserinfo(t *testing.T) {
	_, err := proxy.ResolveURL(googleAPI, "https://user:hunter2@evil.example/x?token=SECRETQ", []string{"k=SECRETFLAG"})

	if err == nil {
		t.Fatal("accepted")
	}
	for _, secret := range []string{"hunter2", "SECRETQ", "SECRETFLAG"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error repeats %q: %v", secret, err)
		}
	}
}
