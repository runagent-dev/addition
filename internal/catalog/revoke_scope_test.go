package catalog

import (
	"strings"
	"testing"
)

func withRevokeScope(scope string) string {
	return strings.Replace(baseProvider, "  client_auth_method: \"body\"\n", "  client_auth_method: \"body\"\n  revoke_scope: "+scope+"\n", 1)
}

func TestRevokeScopeDefaultsToTheToken(t *testing.T) {
	c := mustLoad(t, providerFS(map[string]string{"base.yaml": baseProvider}))

	if got := mustProvider(t, c, "base").OAuth2.RevokeScope; got != "token" {
		t.Errorf("revoke_scope = %q, want token", got)
	}
}

func TestRevokeScopeIsDataAndInherited(t *testing.T) {
	child := "id: child\nname: Child\nauth_type: oauth2\nextends: base\n"
	c := mustLoad(t, providerFS(map[string]string{"base.yaml": withRevokeScope(`"grant"`), "child.yaml": child}))

	for _, id := range []string{"base", "child"} {
		if got := mustProvider(t, c, id).OAuth2.RevokeScope; got != "grant" {
			t.Errorf("%s revoke_scope = %q, want grant", id, got)
		}
	}
}

func TestRevokeScopeRefusesAnythingElse(t *testing.T) {
	for _, value := range []string{`"app"`, `""`, `true`, `1`} {
		wantLoadError(t, providerFS(map[string]string{"x.yaml": withRevokeScope(value)}), "x.yaml", "oauth2.revoke_scope")
	}
}

func TestShippedGoogleRevocationIsGrantWide(t *testing.T) {
	c := shipped(t)

	for _, p := range c.Providers() {
		want := "token"
		if p.IdentityRoot == "google" {
			want = "grant"
		}
		if p.OAuth2.RevokeScope != want {
			t.Errorf("%s revoke_scope = %q, want %q", p.ID, p.OAuth2.RevokeScope, want)
		}
	}
}
