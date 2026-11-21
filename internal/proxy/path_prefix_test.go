package proxy_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/proxy"
)

var driveOnly = []string{"/drive/", "/upload/drive/", "/download/drive/"}

func prefixedAPI(host string, prefixes ...string) catalog.API {
	return catalog.API{BaseURL: "https://" + host, AllowedHosts: []string{host}, AllowedPathPrefixes: prefixes}
}

func TestResolveURLConfinesATargetToThePathPrefixesOnEveryAllowedHost(t *testing.T) {
	api := catalog.API{
		BaseURL: "https://www.googleapis.com", AllowedHosts: []string{"www.googleapis.com", "drive.googleapis.com"}, AllowedPathPrefixes: driveOnly,
	}
	allowed := []string{
		"/drive/v3/files", "/upload/drive/v3/files?uploadType=media", "/download/drive/v3/x", "/drive/v3/files/a%40b",
		"https://www.googleapis.com/drive/v3/files", "https://drive.googleapis.com/drive/v3/files", "https://drive.googleapis.com/upload/drive/v3/files",
	}
	for _, target := range allowed {
		if _, err := proxy.ResolveURL(api, target, nil); err != nil {
			t.Errorf("%s: %v", target, err)
		}
	}
	refused := []string{
		"/calendar/v3/users/me/calendarList", "/", "/x", "/drive", "/Drive/v3/files", "/v1/documents/x",
		"https://www.googleapis.com/calendar/v3/users/me/calendarList", "https://drive.googleapis.com/calendar/v3/users/me/calendarList",
		"https://drive.googleapis.com/", "https://drive.googleapis.com/upload/calendar/v3/x",
	}
	for _, target := range refused {
		if _, err := proxy.ResolveURL(api, target, nil); requestErrorCode(err) != "path-not-allowed" {
			t.Errorf("%s: err = %v, want code path-not-allowed", target, err)
		}
	}
}

func TestResolveURLRefusesAPathAServerMightNormaliseIntoAnotherPrefix(t *testing.T) {
	api := prefixedAPI("www.googleapis.com", driveOnly...)
	for _, target := range []string{
		"/drive/../calendar/v3/users/me/calendarList",
		"/drive/./v3/files",
		"/drive/%2e%2e/calendar/v3/x",
		"/drive/%2E%2E/calendar/v3/x",
		"/drive/.%2e/calendar/v3/x",
		"/drive/..%2fcalendar/v3/x",
		"/drive/..%2Fcalendar/v3/x",
		"/drive%2f..%2fcalendar/v3/x",
		"/drive/..%5ccalendar/v3/x",
		"/drive/..\\calendar/v3/x",
		"/drive/%252e%252e/calendar/v3/x",
		"/drive/..;/calendar/v3/x",
		"/drive//v3/files",
		"/drive/v3/files/%09",
		"/drive/v3/files/%20x",
		"https://www.googleapis.com/drive/../calendar/v3/x",
		"https://www.googleapis.com//calendar/v3/x",
	} {
		t.Run(target, func(t *testing.T) {
			got, err := proxy.ResolveURL(api, target, nil)

			if err == nil {
				t.Fatalf("accepted as %s", got)
			}
			if code := requestErrorCode(err); code != "path-not-allowed" {
				t.Errorf("code = %q (%v), want path-not-allowed", code, err)
			}
		})
	}
}

func TestResolveURLRefusesWhitespaceInsideAPathBeforeLookingAtPrefixes(t *testing.T) {
	api := prefixedAPI("www.googleapis.com", driveOnly...)
	for _, target := range []string{"/drive/v3/files\t", "/drive/ /x", "/drive/v3\n/files", "/drive/..\t/calendar"} {
		if _, err := proxy.ResolveURL(api, target, nil); err == nil {
			t.Errorf("%q was accepted", target)
		}
	}
}

func TestResolveURLWithoutPrefixesKeepsEveryPath(t *testing.T) {
	api := catalog.API{BaseURL: "https://api.example.com"}

	if _, err := proxy.ResolveURL(api, "/anything/at/all", nil); err != nil {
		t.Errorf("err = %v, want any path allowed when the provider lists no prefixes", err)
	}
}

func TestPathRefusalNamesThePrefixesAndNotTheTarget(t *testing.T) {
	api := prefixedAPI("www.googleapis.com", driveOnly...)

	_, err := proxy.ResolveURL(api, "/calendar/v3/x?key=SECRETQ", nil)

	if err == nil || !strings.Contains(err.Error(), "/drive/") || strings.Contains(err.Error(), "SECRETQ") {
		t.Errorf("err = %v, want it to list the allowed prefixes and not repeat the target", err)
	}
}

func TestEngineNeverAttachesATokenToAPathOutsideThePrefixes(t *testing.T) {
	up := newUpstream(t, reply(200, "stolen"))
	cases := map[string]*url.URL{
		"another app's route":     up.url("/calendar/v3/users/me/calendarList"),
		"dot segments":            up.url("/drive/../calendar/v3/x"),
		"encoded dot segments":    up.url("/drive/%2e%2e/calendar/v3/x"),
		"encoded slash":           up.url("/drive/..%2fcalendar/v3/x"),
		"double slash":            up.url("//calendar/v3/x"),
		"literal encoded percent": {Scheme: "http", Host: up.srv.Listener.Addr().String(), Path: "/drive/%2e%2e/calendar/v3/x"},
		"RawPath disagrees with Path": {
			Scheme: "http", Host: up.srv.Listener.Addr().String(), Path: "/calendar/v3/x", RawPath: "/drive/v3/x",
		},
		"opaque":      {Scheme: "http", Opaque: "//" + up.srv.Listener.Addr().String() + "/calendar/v3/x"},
		"empty path":  {Scheme: "http", Host: up.srv.Listener.Addr().String()},
		"relative":    {Scheme: "http", Host: up.srv.Listener.Addr().String(), Path: "drive/v3/x"},
		"only a root": up.url("/"),
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEngine(&fakeRenewer{}, &sleeps{})
			transport := &countingTransport{base: http.DefaultTransport}
			e.Client = &http.Client{Transport: transport}
			e.API.AllowedPathPrefixes = driveOnly

			_, err := e.Do(context.Background(), proxy.Request{Method: "GET", URL: target}, oldAccess)

			if code := requestErrorCode(err); code != "path-not-allowed" && code != "host-not-allowed" {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if transport.calls.Load() != 0 || len(up.seen()) != 0 {
				t.Errorf("transport calls %d, server requests %d; nothing may be sent", transport.calls.Load(), len(up.seen()))
			}
		})
	}
}

func TestEngineSendsAnAllowedPathWithTheToken(t *testing.T) {
	up := newUpstream(t, reply(200, "{}"))
	e := newEngine(&fakeRenewer{}, &sleeps{})
	e.API.AllowedPathPrefixes = driveOnly

	if _, _, err := do(t, e, proxy.Request{Method: "GET", URL: up.url("/drive/v3/files?pageSize=1")}, oldAccess); err != nil {
		t.Fatalf("Do: %v", err)
	}

	if seen := up.seen(); len(seen) != 1 || seen[0].path != "/drive/v3/files" || seen[0].auth != "Bearer "+oldAccess {
		t.Errorf("server saw %+v, want one authorised request to /drive/v3/files", seen)
	}
}

func TestCheckDestinationStaysHostOnlyForTheIdentityURL(t *testing.T) {
	api := catalog.API{AllowedHosts: []string{"www.googleapis.com", "openidconnect.googleapis.com"}, AllowedPathPrefixes: []string{"/calendar/"}}

	if err := proxy.CheckDestination(api, mustParseURL(t, "https://openidconnect.googleapis.com/v1/userinfo"), false); err != nil {
		t.Errorf("the identity URL is validated by host: %v", err)
	}
}
