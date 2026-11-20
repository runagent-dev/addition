package catalog

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

const oauth2Block = `oauth2:
  authorize_url: "https://example.com/auth"
  token_url: "https://example.com/token"
  revoke_url: "https://example.com/revoke"
  refresh_supported: true
  pkce_supported: false
  pkce_required: false
  default_scopes: ["base.read", "base.write"]
  scopes_delimiter: "space"
  authorize_params: {access_type: "offline"}
  token_params: {}
  token_response_format: "json"
  client_auth_method: "body"
`

const apiBlock = `api:
  base_url: "https://example.com"
  auth_header: "Authorization"
  auth_prefix: "Bearer"
  rate_limit:
    requests_per_second: 10
    retry_after_header: "Retry-After"
`

const baseProvider = `id: "base"
name: "Base"
auth_type: "oauth2"
` + oauth2Block + apiBlock

func providerFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys["providers/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func mustLoad(t *testing.T, fsys fstest.MapFS) *Catalog {
	t.Helper()
	c, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func mustProvider(t *testing.T, c *Catalog, id string) *Provider {
	t.Helper()
	p, ok := c.Provider(id)
	if !ok {
		t.Fatalf("provider %q not loaded", id)
	}
	return p
}

func wantLoadError(t *testing.T, fsys fstest.MapFS, fragments ...string) {
	t.Helper()
	_, err := Load(fsys)
	if err == nil {
		t.Fatalf("Load succeeded, want error mentioning %q", fragments)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error %q does not mention %q", err, f)
		}
	}
}

func TestProviderLoadsMinimalConfig(t *testing.T) {
	c := mustLoad(t, providerFS(map[string]string{"base.yaml": baseProvider}))

	p := mustProvider(t, c, "base")
	if p.Name != "Base" || p.AuthType != "oauth2" {
		t.Errorf("got name=%q auth_type=%q", p.Name, p.AuthType)
	}
	if p.OAuth2 == nil || p.OAuth2.AuthorizeURL != "https://example.com/auth" {
		t.Fatalf("oauth2 = %+v", p.OAuth2)
	}
	if p.API.BaseURL != "https://example.com" {
		t.Errorf("api.base_url = %q", p.API.BaseURL)
	}
	if p.Extends != "" {
		t.Errorf("extends = %q, want empty", p.Extends)
	}
}

func TestProviderSkipsTemplatesAndNonYAML(t *testing.T) {
	c := mustLoad(t, providerFS(map[string]string{
		"base.yaml":      baseProvider,
		"_template.yaml": `id: ""`,
		"README.md":      "not yaml",
		"alt.yml":        strings.Replace(baseProvider, `id: "base"`, `id: "alt"`, 1),
	}))

	var ids []string
	for _, p := range c.Providers() {
		ids = append(ids, p.ID)
	}
	if want := []string{"alt", "base"}; !slices.Equal(ids, want) {
		t.Errorf("provider ids = %v, want %v", ids, want)
	}
}

func TestProviderExtendsMerge(t *testing.T) {
	child := `id: "child"
name: "Child"
auth_type: "oauth2"
extends: "base"
oauth2:
  revoke_url: null
  default_scopes: ["child.read"]
  token_params: {}
  authorize_params: {}
api:
  base_url: "https://child.example.com"
  rate_limit:
    requests_per_second: 99
`
	c := mustLoad(t, providerFS(map[string]string{"base.yaml": baseProvider, "child.yaml": child}))
	p := mustProvider(t, c, "child")

	t.Run("id and name come from the child", func(t *testing.T) {
		if p.ID != "child" || p.Name != "Child" {
			t.Errorf("id=%q name=%q", p.ID, p.Name)
		}
	})
	t.Run("extends is recorded", func(t *testing.T) {
		if p.Extends != "base" {
			t.Errorf("extends = %q", p.Extends)
		}
	})
	t.Run("scalars override", func(t *testing.T) {
		if p.API.BaseURL != "https://child.example.com" {
			t.Errorf("api.base_url = %q", p.API.BaseURL)
		}
	})
	t.Run("explicit null overrides", func(t *testing.T) {
		if p.OAuth2.RevokeURL != "" {
			t.Errorf("revoke_url = %q, want empty", p.OAuth2.RevokeURL)
		}
	})
	t.Run("arrays replace, never merge", func(t *testing.T) {
		if want := []string{"child.read"}; !slices.Equal(p.OAuth2.DefaultScopes, want) {
			t.Errorf("default_scopes = %v, want %v", p.OAuth2.DefaultScopes, want)
		}
	})
	t.Run("unspecified fields are inherited", func(t *testing.T) {
		if p.OAuth2.AuthorizeURL != "https://example.com/auth" || p.API.AuthHeader != "Authorization" {
			t.Errorf("authorize_url=%q auth_header=%q", p.OAuth2.AuthorizeURL, p.API.AuthHeader)
		}
	})
	t.Run("nested maps merge recursively", func(t *testing.T) {
		if p.API.RateLimit == nil {
			t.Fatal("api.rate_limit is nil")
		}
		if p.API.RateLimit.RequestsPerSecond != 99 {
			t.Errorf("requests_per_second = %v, want 99", p.API.RateLimit.RequestsPerSecond)
		}
		if p.API.RateLimit.RetryAfterHeader != "Retry-After" {
			t.Errorf("retry_after_header = %q, want inherited", p.API.RateLimit.RetryAfterHeader)
		}
	})
	t.Run("an empty child map keeps the base map", func(t *testing.T) {
		if p.OAuth2.AuthorizeParams["access_type"] != "offline" {
			t.Errorf("authorize_params = %v, want base params", p.OAuth2.AuthorizeParams)
		}
	})
	t.Run("the base is left untouched", func(t *testing.T) {
		base := mustProvider(t, c, "base")
		if base.API.BaseURL != "https://example.com" || len(base.OAuth2.DefaultScopes) != 2 {
			t.Errorf("base was mutated: %+v", base)
		}
	})
}

func TestProviderExtendsChainResolvesInAnyFileOrder(t *testing.T) {
	c := mustLoad(t, providerFS(map[string]string{
		"aaa.yaml": `id: "aaa"
name: "Aaa"
auth_type: "oauth2"
extends: "mmm"
`,
		"mmm.yaml": `id: "mmm"
name: "Mmm"
auth_type: "oauth2"
extends: "zzz"
api:
  base_url: "https://mmm.example.com"
`,
		"zzz.yaml": strings.Replace(baseProvider, `id: "base"`, `id: "zzz"`, 1),
	}))

	p := mustProvider(t, c, "aaa")
	if p.OAuth2.TokenURL != "https://example.com/token" || p.API.BaseURL != "https://mmm.example.com" {
		t.Errorf("token_url=%q base_url=%q", p.OAuth2.TokenURL, p.API.BaseURL)
	}
	for id, want := range map[string]string{"aaa": "zzz", "mmm": "zzz", "zzz": "zzz"} {
		got, err := c.IdentityRoot(id)
		if err != nil || got != want {
			t.Errorf("IdentityRoot(%q) = %q, %v; want %q", id, got, err, want)
		}
	}
}

func TestIdentityRootUnknownProvider(t *testing.T) {
	c := mustLoad(t, providerFS(map[string]string{"base.yaml": baseProvider}))
	if _, err := c.IdentityRoot("ghost"); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("IdentityRoot(ghost) error = %v", err)
	}
}

func TestProviderLoadErrors(t *testing.T) {
	without := func(line string) string {
		return strings.Replace(baseProvider, line+"\n", "", 1)
	}
	replace := func(old, new string) string {
		return strings.Replace(baseProvider, old, new, 1)
	}

	cases := []struct {
		name      string
		files     map[string]string
		fragments []string
	}{
		{"missing id", map[string]string{"noid.yaml": without(`id: "base"`)}, []string{"noid.yaml", "id"}},
		{"duplicate id", map[string]string{"a.yaml": baseProvider, "b.yaml": baseProvider}, []string{"base", "duplicate"}},
		{"invalid yaml", map[string]string{"bad.yaml": "id: [unclosed"}, []string{"bad.yaml"}},
		{"extends cycle", map[string]string{
			"a.yaml": "id: a\nname: A\nauth_type: oauth2\nextends: b\n",
			"b.yaml": "id: b\nname: B\nauth_type: oauth2\nextends: a\n",
		}, []string{"cycle"}},
		{"extends unknown", map[string]string{
			"orphan.yaml": "id: orphan\nname: Orphan\nauth_type: oauth2\nextends: ghost\n",
		}, []string{"orphan.yaml", "ghost"}},
		{"child without name", map[string]string{
			"base.yaml":     baseProvider,
			"nameless.yaml": "id: nameless\nauth_type: oauth2\nextends: base\n",
		}, []string{"nameless.yaml", "name"}},
		{"bad auth_type", map[string]string{"x.yaml": replace(`auth_type: "oauth2"`, `auth_type: "magic"`)}, []string{"x.yaml", "auth_type"}},
		{"typo in authorize_url", map[string]string{"x.yaml": replace("authorize_url:", "authorize_ur:")}, []string{"x.yaml", "oauth2.authorize_url"}},
		{"missing base_url", map[string]string{"x.yaml": without(`  base_url: "https://example.com"`)}, []string{"x.yaml", "api.base_url"}},
		{"bad scopes_delimiter", map[string]string{"x.yaml": replace(`"space"`, `"semicolon"`)}, []string{"oauth2.scopes_delimiter"}},
		{"pkce_supported is a string", map[string]string{"x.yaml": replace("pkce_supported: false", `pkce_supported: "false"`)}, []string{"oauth2.pkce_supported"}},
		{"pkce required but unsupported", map[string]string{"x.yaml": replace("pkce_required: false", "pkce_required: true")}, []string{"pkce_required"}},
		{"empty revoke_url", map[string]string{"x.yaml": replace(`revoke_url: "https://example.com/revoke"`, `revoke_url: ""`)}, []string{"oauth2.revoke_url"}},
		{"incomplete token_response_mapping", map[string]string{"x.yaml": replace(
			`  client_auth_method: "body"`,
			"  client_auth_method: \"body\"\n  token_response_mapping:\n    access_token: \"a.b\"",
		)}, []string{"oauth2.token_response_mapping.refresh_token"}},
	}
	for _, field := range []string{
		"refresh_supported", "pkce_supported", "pkce_required", "default_scopes", "scopes_delimiter",
		"authorize_params", "token_params", "token_response_format", "client_auth_method",
	} {
		var line string
		for _, l := range strings.Split(oauth2Block, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), field+":") {
				line = l
			}
		}
		cases = append(cases, struct {
			name      string
			files     map[string]string
			fragments []string
		}{"missing oauth2." + field, map[string]string{"x.yaml": without(line)}, []string{"oauth2." + field}})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantLoadError(t, providerFS(tc.files), tc.fragments...)
		})
	}
}
