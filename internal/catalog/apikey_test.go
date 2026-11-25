package catalog

import (
	"slices"
	"strings"
	"testing"
)

const apiKeyBlock = `api_key:
  create_url: "https://example.com/settings/tokens"
  instructions:
    - "Open the page."
    - "Create a token."
  token_prefix: "pk_"
  env_var: "EXAMPLE_TOKEN"
  auth_prefix: ""
  identity:
    path: "/me"
    field: "name"
    subject_field: "id"
`

const apiKeyAPIBlock = `api:
  base_url: "https://example.com"
  auth_header: "Authorization"
  auth_prefix: "Bearer"
  probe_path: "/me"
`

const apiKeyOnlyProvider = `id: "tok"
name: "Tok"
auth_type: "api_key"
` + apiKeyBlock + apiKeyAPIBlock

// bothMethodsProvider supports OAuth and a token, and connects with a token by default.
const bothMethodsProvider = `id: "both"
name: "Both"
auth_type: "api_key"
` + oauth2Block + apiKeyBlock + apiBlock

func TestAnAPIKeyProviderLoadsWithoutAnOAuth2Block(t *testing.T) {
	c := mustLoad(t, providerFS(map[string]string{"tok.yaml": apiKeyOnlyProvider}))

	p := mustProvider(t, c, "tok")

	if p.AuthType != MethodAPIKey || p.OAuth2 != nil {
		t.Fatalf("auth_type = %q, oauth2 = %+v; want api_key and no oauth2 block", p.AuthType, p.OAuth2)
	}
	k := p.APIKey
	if k == nil {
		t.Fatal("api_key block not loaded")
	}
	if k.CreateURL != "https://example.com/settings/tokens" || k.TokenPrefix != "pk_" || k.EnvVar != "EXAMPLE_TOKEN" ||
		!slices.Equal(k.Instructions, []string{"Open the page.", "Create a token."}) {
		t.Errorf("api_key = %+v", k)
	}
	if k.Identity == nil || k.Identity.Path != "/me" || k.Identity.Field != "name" || k.Identity.SubjectField != "id" {
		t.Errorf("api_key.identity = %+v", k.Identity)
	}
	if !slices.Equal(p.AuthMethods(), []string{MethodAPIKey}) {
		t.Errorf("AuthMethods = %v", p.AuthMethods())
	}
}

func TestAnExplicitEmptyAuthPrefixMeansTheBareToken(t *testing.T) {
	p := mustProvider(t, mustLoad(t, providerFS(map[string]string{"tok.yaml": apiKeyOnlyProvider})), "tok")

	if p.APIKey.AuthPrefix == nil || *p.APIKey.AuthPrefix != "" {
		t.Fatalf("api_key.auth_prefix = %v, want a pointer to the empty string", p.APIKey.AuthPrefix)
	}
	if got := p.API.AuthPrefixFor(MethodAPIKey); got != "" {
		t.Errorf("api_key prefix = %q, want the bare token", got)
	}
	if got := p.API.AuthPrefixFor(MethodOAuth2); got != "Bearer" {
		t.Errorf("oauth2 prefix = %q, want api.auth_prefix", got)
	}
	if got := p.API.AuthPrefixFor(""); got != "Bearer" {
		t.Errorf("a credential with no recorded method uses %q, want api.auth_prefix", got)
	}
}

func TestAnAbsentAuthPrefixFallsBackToTheAPIOne(t *testing.T) {
	body := strings.Replace(apiKeyOnlyProvider, "  auth_prefix: \"\"\n", "", 1)
	p := mustProvider(t, mustLoad(t, providerFS(map[string]string{"tok.yaml": body})), "tok")

	if p.APIKey.AuthPrefix != nil {
		t.Fatalf("api_key.auth_prefix = %q, want absent", *p.APIKey.AuthPrefix)
	}
	if got := p.API.AuthPrefixFor(MethodAPIKey); got != "Bearer" {
		t.Errorf("api_key prefix = %q, want api.auth_prefix", got)
	}
}

func TestAnAPIKeyAuthPrefixOverridesTheAPIOne(t *testing.T) {
	body := strings.Replace(apiKeyOnlyProvider, "  auth_prefix: \"\"\n", "  auth_prefix: \"Token\"\n", 1)
	p := mustProvider(t, mustLoad(t, providerFS(map[string]string{"tok.yaml": body})), "tok")

	if got := p.API.AuthPrefixFor(MethodAPIKey); got != "Token" {
		t.Errorf("api_key prefix = %q, want Token", got)
	}
}

func TestTheAPIAuthPrefixMayBeEmpty(t *testing.T) {
	body := strings.Replace(apiKeyOnlyProvider, "  auth_prefix: \"Bearer\"\n", "  auth_prefix: \"\"\n", 1)
	body = strings.Replace(body, "  auth_prefix: \"\"\n  identity", "  identity", 1)
	p := mustProvider(t, mustLoad(t, providerFS(map[string]string{"tok.yaml": body})), "tok")

	if got := p.API.AuthPrefixFor(MethodAPIKey); got != "" {
		t.Errorf("prefix = %q, want the bare token", got)
	}
}

func TestAProviderMayOfferBothMethodsWithOneDefault(t *testing.T) {
	p := mustProvider(t, mustLoad(t, providerFS(map[string]string{"both.yaml": bothMethodsProvider})), "both")

	if p.AuthType != MethodAPIKey || p.OAuth2 == nil || p.APIKey == nil {
		t.Fatalf("provider = %+v", p)
	}
	if want := []string{MethodAPIKey, MethodOAuth2}; !slices.Equal(p.AuthMethods(), want) {
		t.Errorf("AuthMethods = %v, want %v", p.AuthMethods(), want)
	}
	if !p.Supports(MethodAPIKey) || !p.Supports(MethodOAuth2) || p.Supports("basic") {
		t.Error("Supports disagrees with the blocks the provider has")
	}
}

func TestAnOAuthOnlyProviderOffersOnlyOAuth(t *testing.T) {
	p := mustProvider(t, mustLoad(t, providerFS(map[string]string{"base.yaml": baseProvider})), "base")

	if !slices.Equal(p.AuthMethods(), []string{MethodOAuth2}) || p.APIKey != nil || p.Supports(MethodAPIKey) {
		t.Errorf("methods = %v, api_key = %+v", p.AuthMethods(), p.APIKey)
	}
}

func TestAnOAuthDefaultProviderMayAlsoOfferATokenWithoutChangingItsDefault(t *testing.T) {
	body := baseProvider + "\n" + apiKeyBlock
	p := mustProvider(t, mustLoad(t, providerFS(map[string]string{"base.yaml": body})), "base")

	if p.AuthType != MethodOAuth2 || !slices.Equal(p.AuthMethods(), []string{MethodAPIKey, MethodOAuth2}) {
		t.Errorf("auth_type = %q, methods = %v", p.AuthType, p.AuthMethods())
	}
}

func TestAPIKeyBlockValidation(t *testing.T) {
	remove := func(line string) string { return strings.Replace(apiKeyOnlyProvider, line, "", 1) }
	replace := func(old, new string) string { return strings.Replace(apiKeyOnlyProvider, old, new, 1) }
	cases := map[string]struct {
		body     string
		fragment string
	}{
		"missing create_url": {remove("  create_url: \"https://example.com/settings/tokens\"\n"), "api_key"},
		"create_url over http": {replace("https://example.com/settings/tokens", "http://example.com/settings/tokens"),
			"api_key.create_url"},
		"create_url with userinfo": {replace("https://example.com/settings/tokens", "https://me:pw@example.com/t"),
			"api_key.create_url"},
		"create_url without a host": {replace("https://example.com/settings/tokens", "https:///tokens"), "api_key.create_url"},
		"empty token_prefix":        {replace("token_prefix: \"pk_\"", "token_prefix: \"\""), "api_key.token_prefix"},
		"token_prefix with a space": {replace("token_prefix: \"pk_\"", "token_prefix: \"pk \""), "api_key.token_prefix"},
		"lowercase env_var":         {replace("EXAMPLE_TOKEN", "example_token"), "api_key.env_var"},
		"unknown key":               {replace("  env_var:", "  scopes: [read]\n  env_var:"), "api_key.scopes"},
		"instructions not a list":   {replace("  instructions:\n    - \"Open the page.\"\n    - \"Create a token.\"\n", "  instructions: \"Open it\"\n"), "api_key.instructions"},
		"auth_prefix not a string":  {replace("auth_prefix: \"\"", "auth_prefix: 5"), "api_key.auth_prefix"},
		"identity not a mapping":    {replace("  identity:\n    path: \"/me\"\n    field: \"name\"\n    subject_field: \"id\"\n", "  identity: \"/me\"\n"), "api_key.identity"},
		"identity without a path":   {replace("    path: \"/me\"\n", ""), "api_key.identity.path"},
		"identity path not absolute": {replace("path: \"/me\"", "path: \"me\""),
			"api_key.identity.path"},
		"identity path is a URL": {replace("path: \"/me\"", "path: \"https://evil.example/me\""),
			"api_key.identity.path"},
		"identity path with dot segments": {replace("path: \"/me\"", "path: \"/a/../me\""), "api_key.identity.path"},
		"identity without a subject":      {replace("    subject_field: \"id\"\n", ""), "api_key.identity.subject_field"},
		"identity unknown key":            {replace("    field: \"name\"\n", "    field: \"name\"\n    method: POST\n"), "api_key.identity.method"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			wantLoadError(t, providerFS(map[string]string{"tok.yaml": tc.body}), "tok.yaml", tc.fragment)
		})
	}
}

func TestAnAPIKeyProviderNeedsAnIdentityOrAProbePath(t *testing.T) {
	noIdentity := strings.Replace(apiKeyOnlyProvider, "  identity:\n    path: \"/me\"\n    field: \"name\"\n    subject_field: \"id\"\n", "", 1)
	if c := mustLoad(t, providerFS(map[string]string{"tok.yaml": noIdentity})); mustProvider(t, c, "tok").APIKey.Identity != nil {
		t.Error("identity should be absent")
	}

	noProbe := strings.Replace(apiKeyOnlyProvider, "  probe_path: \"/me\"\n", "", 1)
	if c := mustLoad(t, providerFS(map[string]string{"tok.yaml": noProbe})); mustProvider(t, c, "tok").APIKey.Identity == nil {
		t.Error("identity should load without a probe path")
	}

	neither := strings.Replace(noIdentity, "  probe_path: \"/me\"\n", "", 1)
	wantLoadError(t, providerFS(map[string]string{"tok.yaml": neither}), "tok.yaml", "api_key")
}

func TestTheIdentityPathMustFallUnderTheAllowedPathPrefixes(t *testing.T) {
	prefixes := strings.Replace(apiKeyOnlyProvider, "  probe_path: \"/me\"\n", "  probe_path: \"/v1/me\"\n  allowed_path_prefixes: [\"/v1/\"]\n", 1)

	wantLoadError(t, providerFS(map[string]string{"tok.yaml": prefixes}), "tok.yaml", "api_key.identity.path")

	fixed := strings.Replace(prefixes, "path: \"/me\"", "path: \"/v1/me\"", 1)
	mustLoad(t, providerFS(map[string]string{"tok.yaml": fixed}))
}

func TestTheDefaultAuthTypeNeedsItsBlock(t *testing.T) {
	apiKeyNoBlock := strings.Replace(bothMethodsProvider, apiKeyBlock, "", 1)
	wantLoadError(t, providerFS(map[string]string{"x.yaml": apiKeyNoBlock}), "x.yaml", "api_key")

	oauthNoBlock := strings.Replace(bothMethodsProvider, "auth_type: \"api_key\"", "auth_type: \"oauth2\"", 1)
	oauthNoBlock = strings.Replace(oauthNoBlock, oauth2Block, "", 1)
	wantLoadError(t, providerFS(map[string]string{"x.yaml": oauthNoBlock}), "x.yaml", "oauth2")
}

func TestAnOAuth2BlockIsValidatedEvenWhenTokenIsTheDefault(t *testing.T) {
	broken := strings.Replace(bothMethodsProvider, "  token_url: \"https://example.com/token\"\n", "", 1)

	wantLoadError(t, providerFS(map[string]string{"x.yaml": broken}), "x.yaml", "oauth2.token_url")
}

func TestAPIKeyBlocksFollowExtendsMergeSemantics(t *testing.T) {
	child := `id: "child"
name: "Child"
auth_type: "api_key"
extends: "both"
api_key:
  token_prefix: "ck_"
  auth_prefix: "Token"
  instructions:
    - "Only step."
`
	c := mustLoad(t, providerFS(map[string]string{"both.yaml": bothMethodsProvider, "child.yaml": child}))

	parent, kid := mustProvider(t, c, "both").APIKey, mustProvider(t, c, "child").APIKey

	if kid.TokenPrefix != "ck_" || kid.AuthPrefix == nil || *kid.AuthPrefix != "Token" {
		t.Errorf("scalars must override: %+v", kid)
	}
	if !slices.Equal(kid.Instructions, []string{"Only step."}) {
		t.Errorf("an array must replace, got %v", kid.Instructions)
	}
	if kid.CreateURL != parent.CreateURL || kid.Identity == nil || kid.Identity.Path != "/me" {
		t.Errorf("unspecified fields must be inherited: %+v", kid)
	}
	if parent.TokenPrefix != "pk_" || *parent.AuthPrefix != "" {
		t.Errorf("the parent changed: %+v", parent)
	}
}

func TestAChildMayOverrideAParentsPrefixWithTheBareToken(t *testing.T) {
	parent := strings.Replace(bothMethodsProvider, "  auth_prefix: \"\"\n", "  auth_prefix: \"Token\"\n", 1)
	child := "id: \"child\"\nname: \"Child\"\nauth_type: \"api_key\"\nextends: \"both\"\napi_key:\n  auth_prefix: \"\"\n"
	c := mustLoad(t, providerFS(map[string]string{"both.yaml": parent, "child.yaml": child}))

	got := mustProvider(t, c, "child").APIKey.AuthPrefix

	if got == nil || *got != "" {
		t.Errorf("auth_prefix = %v, want the explicit empty string to survive the merge", got)
	}
}

func TestShippedGitHubOffersATokenByDefaultAndKeepsOAuth(t *testing.T) {
	p := mustProvider(t, shipped(t), "github")

	if p.AuthType != MethodAPIKey || p.OAuth2 == nil {
		t.Fatalf("auth_type = %q, oauth2 = %v; want api_key by default and the oauth2 block kept", p.AuthType, p.OAuth2)
	}
	if want := []string{MethodAPIKey, MethodOAuth2}; !slices.Equal(p.AuthMethods(), want) {
		t.Errorf("AuthMethods = %v, want %v", p.AuthMethods(), want)
	}
	k := p.APIKey
	if k == nil || k.CreateURL != "https://github.com/settings/tokens" || k.TokenPrefix != "" || k.AuthPrefix != nil {
		t.Fatalf("api_key = %+v", k)
	}
	if k.Identity == nil || k.Identity.Path != "/user" || k.Identity.SubjectField != "id" || k.Identity.Field != "login" {
		t.Errorf("api_key.identity = %+v", k.Identity)
	}
	if p.API.ProbePath != "/user" || p.API.AuthHeader != "Authorization" || p.API.AuthPrefixFor(MethodAPIKey) != "Bearer" {
		t.Errorf("api = %+v", p.API)
	}
	if len(k.Instructions) == 0 {
		t.Error("github gives no instructions for creating a token")
	}
}

func TestShippedGoogleAppsStayOAuthOnly(t *testing.T) {
	for _, id := range []string{"google", "gmail", "google-drive", "google-calendar", "google-chat", "google-docs", "slack"} {
		p := mustProvider(t, shipped(t), id)
		if p.APIKey != nil || !slices.Equal(p.AuthMethods(), []string{MethodOAuth2}) {
			t.Errorf("%s: methods = %v, api_key = %+v", id, p.AuthMethods(), p.APIKey)
		}
	}
}

func TestTheEffectiveVerificationPathIsValidatedAfterTheMerge(t *testing.T) {
	noIdentity := func(probe string) string {
		body := strings.Replace(apiKeyOnlyProvider, "  identity:\n    path: \"/me\"\n    field: \"name\"\n    subject_field: \"id\"\n", "", 1)
		return strings.Replace(body, "  probe_path: \"/me\"\n", "  probe_path: \""+probe+"\"\n  allowed_path_prefixes: [\"/v1/\"]\n", 1)
	}
	mustLoad(t, providerFS(map[string]string{"tok.yaml": noIdentity("/v1/me")}))
	mustLoad(t, providerFS(map[string]string{"tok.yaml": noIdentity("/v1/me?fields=id")}))

	for name, probe := range map[string]string{
		"outside the prefixes": "/me", "a dot segment": "/v1/../me", "a double slash": "/v1//me", "an escaped slash": "/v1/a%2fb",
	} {
		t.Run(name, func(t *testing.T) {
			wantLoadError(t, providerFS(map[string]string{"tok.yaml": noIdentity(probe)}), "tok.yaml", "probe_path")
		})
	}
}

func TestAChildInheritingTheProbePathIsCheckedAgainstItsOwnPrefixes(t *testing.T) {
	parent := strings.Replace(apiKeyOnlyProvider, "  identity:\n    path: \"/me\"\n    field: \"name\"\n    subject_field: \"id\"\n", "", 1)
	child := "id: \"child\"\nname: \"Child\"\nauth_type: \"api_key\"\nextends: \"tok\"\napi:\n  allowed_path_prefixes: [\"/v2/\"]\n"

	wantLoadError(t, providerFS(map[string]string{"tok.yaml": parent, "child.yaml": child}), "child.yaml", "probe_path")
}
