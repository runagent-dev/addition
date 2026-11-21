package catalog

import (
	"slices"
	"strings"
	"testing"
)

const prefixesAPIBlock = `api:
  base_url: "https://example.com/v1"
  allowed_hosts: ["example.com", "upload.example.com"]
  allowed_path_prefixes: ["/drive/", "/upload/drive/"]
  auth_header: "Authorization"
  auth_prefix: "Bearer"
`

func TestAllowedPathPrefixesAreData(t *testing.T) {
	c := mustLoad(t, providerFS(map[string]string{"base.yaml": withHosts(prefixesAPIBlock)}))

	p := mustProvider(t, c, "base")

	if want := []string{"/drive/", "/upload/drive/"}; !slices.Equal(p.API.AllowedPathPrefixes, want) {
		t.Errorf("allowed_path_prefixes = %v, want %v", p.API.AllowedPathPrefixes, want)
	}
}

func TestAProviderWithoutPathPrefixesAllowsEveryPath(t *testing.T) {
	c := mustLoad(t, providerFS(map[string]string{"base.yaml": baseProvider}))

	if got := mustProvider(t, c, "base").API.AllowedPathPrefixes; len(got) != 0 {
		t.Errorf("allowed_path_prefixes = %v, want none", got)
	}
}

func TestPathPrefixesReplaceUnderExtends(t *testing.T) {
	child := `id: "child"
name: "Child"
auth_type: "oauth2"
extends: "base"
api:
  allowed_path_prefixes: ["/calendar/"]
`
	files := map[string]string{"base.yaml": withHosts(prefixesAPIBlock), "child.yaml": child}

	got := mustProvider(t, mustLoad(t, providerFS(files)), "child").API.AllowedPathPrefixes

	if want := []string{"/calendar/"}; !slices.Equal(got, want) {
		t.Errorf("an overriding array must replace: got %v, want %v", got, want)
	}
}

func TestPathPrefixValidation(t *testing.T) {
	api := func(prefixes string) string {
		return strings.Replace(prefixesAPIBlock, `["/drive/", "/upload/drive/"]`, prefixes, 1)
	}
	cases := []struct{ name, prefixes string }{
		{"not a list", `"/drive/"`},
		{"not strings", `[1]`},
		{"empty entry", `[""]`},
		{"no leading slash", `["drive/"]`},
		{"dot segment", `["/drive/../calendar/"]`},
		{"single dot segment", `["/drive/./"]`},
		{"double slash", `["/drive//"]`},
		{"backslash", `["/drive\\"]`},
		{"percent escape", `["/drive%2f"]`},
		{"query", `["/drive/?x=1"]`},
		{"fragment", `["/drive/#x"]`},
		{"space", `["/my drive/"]`},
		{"absolute URL", `["https://example.com/drive/"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantLoadError(t, providerFS(map[string]string{"x.yaml": withHosts(api(tc.prefixes))}), "x.yaml", "api.allowed_path_prefixes")
		})
	}
}

func TestAnEmptyPathPrefixListClearsAnInheritedOne(t *testing.T) {
	child := `id: "child"
name: "Child"
auth_type: "oauth2"
extends: "base"
api:
  allowed_path_prefixes: []
`

	got := mustProvider(t, mustLoad(t, providerFS(map[string]string{"base.yaml": withHosts(prefixesAPIBlock), "child.yaml": child})), "child")

	if len(got.API.AllowedPathPrefixes) != 0 {
		t.Errorf("allowed_path_prefixes = %v, want none", got.API.AllowedPathPrefixes)
	}
}

func TestPlainPathRefusesAnythingAServerMightNormaliseIntoAnotherRoute(t *testing.T) {
	plain := []string{"/", "/drive/v3/files", "/drive/", "/v1/documents/abc:batchUpdate", "/calendar/v3/calendars/me%40example.com/events", "/a/b/"}
	notPlain := []string{
		"", "drive/v3", "//calendar", "/drive//v3", "/drive/../calendar/v3", "/drive/./v3", "/drive/..", "/..", "/drive/%2e%2e/calendar",
		"/drive/%2E%2E/calendar", "/drive/.%2e/calendar", "/drive/..%2fcalendar", "/drive/..%2Fcalendar", "/drive%2f..%2fcalendar",
		"/drive/..%5ccalendar", "/drive\\..\\calendar", "/drive/%252e%252e/calendar", "/drive/..;/calendar", "/drive/.;x/calendar",
		"/drive/%09/x", "/drive/%20/x", "/drive/x y", "/drive/\t", "/drive/%00", "/drive/%zz", "/drive/%",
	}
	for _, p := range plain {
		if !PlainPath(p) {
			t.Errorf("PlainPath(%q) = false, want true", p)
		}
	}
	for _, p := range notPlain {
		if PlainPath(p) {
			t.Errorf("PlainPath(%q) = true, want false", p)
		}
	}
}

func TestShippedGoogleAppsAreConfinedToTheirOwnRoutes(t *testing.T) {
	c := shipped(t)
	want := map[string][]string{
		"google-drive":    {"/drive/", "/upload/drive/", "/download/drive/"},
		"google-calendar": {"/calendar/"},
		"google-docs":     {"/v1/documents"},
		"google-chat":     {"/v1/", "/upload/v1/"},
		"gmail":           {"/gmail/", "/upload/gmail/"},
	}

	for id, prefixes := range want {
		if got := mustProvider(t, c, id).API.AllowedPathPrefixes; !slices.Equal(got, prefixes) {
			t.Errorf("%s allowed_path_prefixes = %v, want %v", id, got, prefixes)
		}
	}
}

func TestAppsSharingAHostNeverShareARoute(t *testing.T) {
	c := shipped(t)
	apps := c.Apps("google")

	for i, a := range apps {
		for _, b := range apps[i+1:] {
			if !sharesHost(a.API.Hosts(), b.API.Hosts()) {
				continue
			}
			for _, x := range a.API.AllowedPathPrefixes {
				for _, y := range b.API.AllowedPathPrefixes {
					if strings.HasPrefix(x, y) || strings.HasPrefix(y, x) {
						t.Errorf("%s %q and %s %q overlap on a shared host", a.ID, x, b.ID, y)
					}
				}
			}
			if len(a.API.AllowedPathPrefixes) == 0 || len(b.API.AllowedPathPrefixes) == 0 {
				t.Errorf("%s and %s share a host, so each needs allowed_path_prefixes", a.ID, b.ID)
			}
		}
	}
}

func sharesHost(a, b []string) bool {
	for _, host := range a {
		if host != googleIdentityHost && slices.Contains(b, host) {
			return true
		}
	}
	return false
}
