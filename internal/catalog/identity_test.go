package catalog

import (
	"strings"
	"testing"
)

const identityBlock = `  identity:
    url: "https://example.com/userinfo"
    field: "profile.email"
`

func withIdentity(block string) string {
	return strings.Replace(baseProvider, "  client_auth_method: \"body\"\n", "  client_auth_method: \"body\"\n"+block, 1)
}

func TestProviderIdentityIsOptional(t *testing.T) {
	c := mustLoad(t, providerFS(map[string]string{"base.yaml": baseProvider}))
	if id := mustProvider(t, c, "base").OAuth2.Identity; id != nil {
		t.Errorf("identity = %+v, want nil", id)
	}
}

func TestProviderIdentityLoadsAndIsInherited(t *testing.T) {
	child := "id: child\nname: Child\nauth_type: oauth2\nextends: base\n"
	c := mustLoad(t, providerFS(map[string]string{"base.yaml": withIdentity(identityBlock), "child.yaml": child}))

	for _, id := range []string{"base", "child"} {
		got := mustProvider(t, c, id).OAuth2.Identity
		if got == nil || got.URL != "https://example.com/userinfo" || got.Field != "profile.email" {
			t.Errorf("%s identity = %+v", id, got)
		}
	}
}

func TestProviderIdentityValidation(t *testing.T) {
	cases := map[string]struct {
		block    string
		fragment string
	}{
		"not a mapping": {"  identity: \"https://example.com\"\n", "oauth2.identity"},
		"missing url":   {"  identity:\n    field: email\n", "oauth2.identity.url"},
		"empty field":   {"  identity:\n    url: \"https://x\"\n    field: \"\"\n", "oauth2.identity.field"},
		"unknown key":   {"  identity:\n    url: \"https://x\"\n    field: email\n    method: POST\n", "oauth2.identity.method"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			wantLoadError(t, providerFS(map[string]string{"x.yaml": withIdentity(tc.block)}), "x.yaml", tc.fragment)
		})
	}
}

func TestShippedGoogleIdentity(t *testing.T) {
	c := shipped(t)
	for _, id := range []string{"google", "gmail", "google-drive", "google-calendar"} {
		got := mustProvider(t, c, id).OAuth2.Identity
		if got == nil || got.URL != "https://openidconnect.googleapis.com/v1/userinfo" || got.Field != "email" {
			t.Errorf("%s identity = %+v", id, got)
		}
	}
}
