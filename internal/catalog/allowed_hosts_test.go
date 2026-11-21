package catalog

import (
	"slices"
	"strings"
	"testing"
)

const hostsAPIBlock = `api:
  base_url: "https://example.com/v1"
  allowed_hosts: ["example.com", "upload.example.com"]
  auth_header: "Authorization"
  auth_prefix: "Bearer"
`

func withHosts(api string) string {
	return strings.Replace(baseProvider, apiBlock, api, 1)
}

func TestAllowedHostsAreData(t *testing.T) {
	c := mustLoad(t, providerFS(map[string]string{"base.yaml": withHosts(hostsAPIBlock)}))

	p := mustProvider(t, c, "base")

	if want := []string{"example.com", "upload.example.com"}; !slices.Equal(p.API.AllowedHosts, want) {
		t.Errorf("allowed_hosts = %v, want %v", p.API.AllowedHosts, want)
	}
	if got := p.API.Hosts(); !slices.Equal(got, p.API.AllowedHosts) {
		t.Errorf("Hosts() = %v, want the allowed_hosts", got)
	}
}

func TestHostsDefaultsToTheBaseURLHost(t *testing.T) {
	c := mustLoad(t, providerFS(map[string]string{"base.yaml": baseProvider}))

	p := mustProvider(t, c, "base")

	if got, want := p.API.Hosts(), []string{"example.com"}; !slices.Equal(got, want) {
		t.Errorf("Hosts() = %v, want %v", got, want)
	}
}

func TestAllowedHostsFollowExtendsSemantics(t *testing.T) {
	child := `id: "child"
name: "Child"
auth_type: "oauth2"
extends: "base"
api:
  base_url: "https://upload.example.com"
`
	override := child + `  allowed_hosts: ["upload.example.com"]` + "\n"
	files := map[string]string{"base.yaml": withHosts(hostsAPIBlock)}

	inherited := mustProvider(t, mustLoad(t, providerFS(map[string]string{"base.yaml": files["base.yaml"], "child.yaml": child})), "child")
	replaced := mustProvider(t, mustLoad(t, providerFS(map[string]string{"base.yaml": files["base.yaml"], "child.yaml": override})), "child")

	if want := []string{"example.com", "upload.example.com"}; !slices.Equal(inherited.API.AllowedHosts, want) {
		t.Errorf("inherited = %v, want %v", inherited.API.AllowedHosts, want)
	}
	if want := []string{"upload.example.com"}; !slices.Equal(replaced.API.AllowedHosts, want) {
		t.Errorf("an overriding array must replace: got %v, want %v", replaced.API.AllowedHosts, want)
	}
}

func TestAllowedHostsValidation(t *testing.T) {
	api := func(hosts string) string {
		return strings.Replace(hostsAPIBlock, `["example.com", "upload.example.com"]`, hosts, 1)
	}
	cases := []struct {
		name string
		api  string
		want string
	}{
		{"not a list", api(`"example.com"`), "api.allowed_hosts"},
		{"entry with a scheme", api(`["https://example.com"]`), "api.allowed_hosts"},
		{"entry with a path", api(`["example.com/v1"]`), "api.allowed_hosts"},
		{"uppercase entry", api(`["Example.com"]`), "api.allowed_hosts"},
		{"entry with userinfo", api(`["me@example.com"]`), "api.allowed_hosts"},
		{"empty entry", api(`[""]`), "api.allowed_hosts"},
		{"empty list", api(`[]`), "api.allowed_hosts"},
		{"base_url host not listed", api(`["upload.example.com"]`), "api.base_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantLoadError(t, providerFS(map[string]string{"x.yaml": withHosts(tc.api)}), "x.yaml", tc.want)
		})
	}
}

func TestShippedProvidersListTheirAPIHosts(t *testing.T) {
	c := shipped(t)

	google := mustProvider(t, c, "google")
	for _, host := range []string{
		"www.googleapis.com", "gmail.googleapis.com", "docs.googleapis.com", "sheets.googleapis.com",
	} {
		if !slices.Contains(google.API.Hosts(), host) {
			t.Errorf("google hosts %v lack %s", google.API.Hosts(), host)
		}
	}
	for _, id := range []string{"gmail", "google-drive", "google-calendar"} {
		if got := mustProvider(t, c, id).API.Hosts(); !slices.Equal(got, google.API.Hosts()) {
			t.Errorf("%s hosts = %v, want google's %v", id, got, google.API.Hosts())
		}
	}
	for _, p := range c.Providers() {
		for _, host := range p.API.Hosts() {
			if strings.ContainsAny(host, "/@: ") || host != strings.ToLower(host) {
				t.Errorf("%s lists a malformed host %q", p.ID, host)
			}
		}
	}
}

const portAPIBlock = `api:
  base_url: "https://example.com:8443/v1"
  allowed_hosts: ["example.com:8443"]
  auth_header: "Authorization"
  auth_prefix: "Bearer"
`

func TestHostPortsAreRefusedBecauseTokensGoToPort443Only(t *testing.T) {
	cases := map[string]string{
		"a listed port":                portAPIBlock,
		"an explicit 443":              strings.Replace(portAPIBlock, "8443", "443", 2),
		"a base_url port without list": strings.Replace(portAPIBlock, `  allowed_hosts: ["example.com:8443"]`+"\n", "", 1),
		"a listed port beside a plain": strings.Replace(hostsAPIBlock, `"upload.example.com"`, `"upload.example.com:8443"`, 1),
	}
	for name, api := range cases {
		t.Run(name, func(t *testing.T) {
			wantLoadError(t, providerFS(map[string]string{"x.yaml": withHosts(api)}), "x.yaml", "port")
		})
	}
}

func TestHostPortsLoadWhenTheLoopbackOptionIsOn(t *testing.T) {
	fsys := providerFS(map[string]string{"x.yaml": strings.Replace(baseProvider, apiBlock, portAPIBlock, 1)})

	c, err := LoadWithOptions(fsys, Options{AllowHostPorts: true})

	if err != nil {
		t.Fatalf("LoadWithOptions: %v", err)
	}
	if got := mustProvider(t, c, "base").API.Hosts(); !slices.Equal(got, []string{"example.com:8443"}) {
		t.Errorf("Hosts() = %v", got)
	}
}
