package catalog

import (
	"slices"
	"testing"
)

const googleIdentityHost = "openidconnect.googleapis.com"

// shippedAppHosts is every host each Google app's access token may reach, and no other.
var shippedAppHosts = map[string][]string{
	"gmail":           {"gmail.googleapis.com", googleIdentityHost},
	"google-drive":    {"www.googleapis.com", "drive.googleapis.com", googleIdentityHost},
	"google-calendar": {"www.googleapis.com", googleIdentityHost},
	"google-chat":     {"chat.googleapis.com", googleIdentityHost},
	"google-docs":     {"docs.googleapis.com", googleIdentityHost},
}

func providerIDs(providers []*Provider) []string {
	var ids []string
	for _, p := range providers {
		ids = append(ids, p.ID)
	}
	return ids
}

func TestProviderRecordsItsIdentityRoot(t *testing.T) {
	c := shipped(t)

	for id, root := range map[string]string{"google": "google", "gmail": "google", "google-chat": "google", "github": "github", "slack": "slack"} {
		if got := mustProvider(t, c, id).IdentityRoot; got != root {
			t.Errorf("%s IdentityRoot = %q, want %q", id, got, root)
		}
	}
}

func TestAppsAreTheProvidersNothingExtends(t *testing.T) {
	c := shipped(t)

	wantGoogle := []string{"gmail", "google-calendar", "google-chat", "google-docs", "google-drive"}
	if got := providerIDs(c.Apps("google")); !slices.Equal(got, wantGoogle) {
		t.Errorf("Apps(google) = %v, want %v", got, wantGoogle)
	}
	if got := providerIDs(c.Apps("github")); !slices.Equal(got, []string{"github"}) {
		t.Errorf("Apps(github) = %v, want just github", got)
	}
	for id, want := range map[string]bool{"google": false, "gmail": true, "google-chat": true, "github": true, "slack": true, "nope": false} {
		if got := c.Connectable(id); got != want {
			t.Errorf("Connectable(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestShippedGoogleAppsAreScopedToTheirOwnHosts(t *testing.T) {
	c := shipped(t)

	for id, want := range shippedAppHosts {
		got := mustProvider(t, c, id).API.Hosts()
		if !slices.Equal(got, want) {
			t.Errorf("%s allowed_hosts = %v, want %v", id, got, want)
		}
	}
}

func TestShippedGoogleChatAndDocs(t *testing.T) {
	c := shipped(t)
	const api = "https://www.googleapis.com/auth/"

	chat := mustProvider(t, c, "google-chat")
	wantChat := []string{
		"openid", api + "userinfo.email", api + "chat.spaces.readonly", api + "chat.messages.readonly",
		api + "chat.memberships.readonly", api + "chat.messages.create",
	}
	if chat.Name != "Google Chat" || !slices.Equal(chat.OAuth2.DefaultScopes, wantChat) {
		t.Errorf("google-chat = %q with scopes %v, want %v", chat.Name, chat.OAuth2.DefaultScopes, wantChat)
	}
	docs := mustProvider(t, c, "google-docs")
	if want := []string{"openid", api + "userinfo.email", api + "documents"}; docs.Name != "Google Docs" || !slices.Equal(docs.OAuth2.DefaultScopes, want) {
		t.Errorf("google-docs = %q with scopes %v, want %v", docs.Name, docs.OAuth2.DefaultScopes, want)
	}
	for _, id := range []string{"google-chat", "google-docs"} {
		if _, ok := c.Toolkit(id); ok {
			t.Errorf("%s has a toolkit; its tools belong to milestone M5", id)
		}
	}
}

func TestShippedProvidersNeverWidenAGrantBeyondTheRequestedScopes(t *testing.T) {
	c := shipped(t)

	for _, p := range c.Providers() {
		if _, widens := p.OAuth2.AuthorizeParams["include_granted_scopes"]; widens {
			t.Errorf("%s sets include_granted_scopes, which widens a token past the app's scopes", p.ID)
		}
	}
}

func TestAnAppThatNamesNoHostsInheritsOnlyTheRootsNarrowList(t *testing.T) {
	c := shipped(t)

	root := mustProvider(t, c, "google").API.Hosts()
	if slices.Contains(root, "drive.googleapis.com") || slices.Contains(root, "gmail.googleapis.com") || slices.Contains(root, "chat.googleapis.com") {
		t.Errorf("the google root lists app hosts %v; an app that forgets allowed_hosts would inherit them", root)
	}
}
