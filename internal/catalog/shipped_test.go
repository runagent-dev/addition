package catalog

import (
	"slices"
	"testing"

	"github.com/runagent-dev/addition/shared"
)

func shipped(t *testing.T) *Catalog {
	t.Helper()
	c, err := Load(shared.FS)
	if err != nil {
		t.Fatalf("Load(shared.FS): %v", err)
	}
	return c
}

func shippedTool(t *testing.T, c *Catalog, provider, id string) Tool {
	t.Helper()
	tk, ok := c.Toolkit(provider)
	if !ok {
		t.Fatalf("toolkit %q not loaded", provider)
	}
	for _, tool := range tk.Tools {
		if tool.ID == id {
			return tool
		}
	}
	t.Fatalf("tool %s/%s not found", provider, id)
	return Tool{}
}

func TestShippedProviders(t *testing.T) {
	c := shipped(t)

	var ids []string
	for _, p := range c.Providers() {
		ids = append(ids, p.ID)
	}
	want := []string{"github", "gmail", "google", "google-calendar", "google-drive", "slack"}
	if !slices.Equal(ids, want) {
		t.Errorf("provider ids = %v, want %v", ids, want)
	}
}

func TestShippedGoogleFamilyExtendsGoogle(t *testing.T) {
	c := shipped(t)
	google := mustProvider(t, c, "google")

	for id, baseURL := range map[string]string{
		"gmail":           "https://gmail.googleapis.com",
		"google-calendar": "https://www.googleapis.com/calendar/v3",
		"google-drive":    "https://www.googleapis.com",
	} {
		t.Run(id, func(t *testing.T) {
			p := mustProvider(t, c, id)
			if p.Extends != "google" {
				t.Errorf("extends = %q", p.Extends)
			}
			if root, err := c.IdentityRoot(id); err != nil || root != "google" {
				t.Errorf("IdentityRoot = %q, %v; want google", root, err)
			}
			if p.API.BaseURL != baseURL {
				t.Errorf("api.base_url = %q, want %q", p.API.BaseURL, baseURL)
			}
			if p.OAuth2.AuthorizeURL != google.OAuth2.AuthorizeURL || p.OAuth2.TokenURL != google.OAuth2.TokenURL {
				t.Errorf("oauth endpoints not inherited: %+v", p.OAuth2)
			}
			if p.OAuth2.AuthorizeParams["access_type"] != "offline" || p.OAuth2.AuthorizeParams["prompt"] != "consent" {
				t.Errorf("authorize_params = %v", p.OAuth2.AuthorizeParams)
			}
			if p.API.AuthHeader != "Authorization" || p.API.AuthPrefix != "Bearer" {
				t.Errorf("api auth header not inherited: %+v", p.API)
			}
			if slices.Contains(p.OAuth2.DefaultScopes, "https://www.googleapis.com/auth/userinfo.profile") {
				t.Errorf("default_scopes merged with google's instead of replacing: %v", p.OAuth2.DefaultScopes)
			}
		})
	}
}

func TestShippedDriveScopes(t *testing.T) {
	drive := mustProvider(t, shipped(t), "google-drive")
	want := []string{
		"openid",
		"https://www.googleapis.com/auth/userinfo.email",
		"https://www.googleapis.com/auth/drive",
	}
	if !slices.Equal(drive.OAuth2.DefaultScopes, want) {
		t.Errorf("default_scopes = %v, want %v", drive.OAuth2.DefaultScopes, want)
	}
}

func TestShippedStandaloneProvidersAreTheirOwnRoot(t *testing.T) {
	c := shipped(t)
	for _, id := range []string{"google", "github", "slack"} {
		if root, err := c.IdentityRoot(id); err != nil || root != id {
			t.Errorf("IdentityRoot(%q) = %q, %v", id, root, err)
		}
	}
	github := mustProvider(t, c, "github")
	if github.OAuth2.RefreshSupported || !slices.Equal(github.OAuth2.DefaultScopes, []string{"repo", "read:user", "user:email"}) {
		t.Errorf("github oauth2 = %+v", github.OAuth2)
	}
}

func TestShippedTokenResponseMapping(t *testing.T) {
	c := shipped(t)
	for _, id := range []string{"google", "gmail", "github"} {
		if m := mustProvider(t, c, id).OAuth2.TokenResponseMapping; m != nil {
			t.Errorf("%s token_response_mapping = %+v, want unset", id, m)
		}
	}
	want := TokenResponseMapping{
		AccessToken:  "authed_user.access_token",
		RefreshToken: "authed_user.refresh_token",
		ExpiresIn:    "authed_user.expires_in",
	}
	if m := mustProvider(t, c, "slack").OAuth2.TokenResponseMapping; m == nil || *m != want {
		t.Errorf("slack token_response_mapping = %+v", m)
	}
	if d := mustProvider(t, c, "slack").OAuth2.ScopesDelimiter; d != "comma" {
		t.Errorf("slack scopes_delimiter = %q", d)
	}
}

func TestShippedToolkitsAndCommands(t *testing.T) {
	c := shipped(t)
	want := map[string]struct {
		command string
		tools   []string
	}{
		"github":          {"github", []string{"list_repos", "list_issues", "create_issue", "list_pull_requests", "create_pull_request"}},
		"gmail":           {"gmail", []string{"list_messages", "get_message", "send_message", "search_messages", "modify_message"}},
		"google-calendar": {"calendar", []string{"list_events", "get_event", "create_event", "update_event", "delete_event"}},
		"google-drive":    {"drive", []string{"list_files", "get_file", "create_folder", "update_file", "trash_file"}},
		"slack":           {"slack", []string{"send_message", "list_channels", "list_messages"}},
	}

	var providers []string
	for _, tk := range c.Toolkits() {
		providers = append(providers, tk.Provider)
	}
	if !slices.Equal(providers, []string{"github", "gmail", "google-calendar", "google-drive", "slack"}) {
		t.Errorf("toolkit providers = %v", providers)
	}
	for provider, w := range want {
		tk, ok := c.Toolkit(provider)
		if !ok {
			t.Errorf("toolkit %q missing", provider)
			continue
		}
		var ids []string
		for _, tool := range tk.Tools {
			ids = append(ids, tool.ID)
		}
		if tk.Command != w.command || !slices.Equal(ids, w.tools) {
			t.Errorf("%s: command=%q tools=%v, want %q %v", provider, tk.Command, ids, w.command, w.tools)
		}
	}
}

func TestShippedEffects(t *testing.T) {
	c := shipped(t)
	want := map[string]map[string]Effect{
		"gmail": {
			"list_messages": EffectRead, "get_message": EffectRead, "search_messages": EffectRead,
			"send_message": EffectWrite, "modify_message": EffectWrite,
		},
		"google-calendar": {
			"list_events": EffectRead, "get_event": EffectRead,
			"create_event": EffectWrite, "update_event": EffectWrite, "delete_event": EffectDestructive,
		},
		"google-drive": {
			"list_files": EffectRead, "get_file": EffectRead,
			"create_folder": EffectWrite, "update_file": EffectWrite, "trash_file": EffectDestructive,
		},
		"github": {
			"list_repos": EffectRead, "list_issues": EffectRead, "list_pull_requests": EffectRead,
			"create_issue": EffectWrite, "create_pull_request": EffectWrite,
		},
		"slack": {"list_channels": EffectRead, "list_messages": EffectRead, "send_message": EffectWrite},
	}
	for provider, tools := range want {
		for id, effect := range tools {
			if got := shippedTool(t, c, provider, id).Effect; got != effect {
				t.Errorf("%s/%s effect = %q, want %q", provider, id, got, effect)
			}
		}
	}
}

func TestShippedDriveTools(t *testing.T) {
	c := shipped(t)
	ops := map[string]string{
		"list_files":    "GET /drive/v3/files",
		"get_file":      "GET /drive/v3/files/{fileId}",
		"create_folder": "POST /drive/v3/files",
		"update_file":   "PATCH /drive/v3/files/{fileId}",
		"trash_file":    "PATCH /drive/v3/files/{fileId}",
	}
	for id, op := range ops {
		if got := shippedTool(t, c, "google-drive", id).Operation; got != op {
			t.Errorf("%s operation = %q, want %q", id, got, op)
		}
	}

	list := shippedTool(t, c, "google-drive", "list_files")
	for _, name := range []string{"q", "pageSize", "fields"} {
		if !slices.ContainsFunc(list.Params, func(p Param) bool { return p.Name == name && !p.Required }) {
			t.Errorf("list_files lacks optional param %q", name)
		}
	}
	if p := list.Pagination; p == nil || p.Type != "cursor" || p.CursorParam != "pageToken" || p.CursorResponseField != "nextPageToken" {
		t.Errorf("list_files pagination = %+v", p)
	}

	update := shippedTool(t, c, "google-drive", "update_file")
	for _, name := range []string{"fileId", "name", "addParents", "removeParents"} {
		if !slices.ContainsFunc(update.Params, func(p Param) bool { return p.Name == name }) {
			t.Errorf("update_file lacks param %q", name)
		}
	}
}

func TestShippedPaginationConventions(t *testing.T) {
	c := shipped(t)
	if f := shippedTool(t, c, "gmail", "list_messages").Pagination.CursorResponseField; f != "nextPageToken" {
		t.Errorf("gmail list_messages cursor field = %q", f)
	}
	if f := shippedTool(t, c, "slack", "list_channels").Pagination.CursorResponseField; f != "response_metadata.next_cursor" {
		t.Errorf("slack list_channels cursor field = %q", f)
	}
	repos := shippedTool(t, c, "github", "list_repos").Pagination
	if repos.Type != "page" || repos.CursorResponseField != "" {
		t.Errorf("github list_repos pagination = %+v", repos)
	}
}

func TestShippedGmailToolShape(t *testing.T) {
	c := shipped(t)
	send := shippedTool(t, c, "gmail", "send_message")
	if send.Transform != "gmail:rfc2822" || send.Method != "POST" || send.Path != "/gmail/v1/users/me/messages/send" {
		t.Errorf("send_message = %+v", send)
	}
	get := shippedTool(t, c, "gmail", "get_message")
	i := slices.IndexFunc(get.Params, func(p Param) bool { return p.Name == "format" })
	if i < 0 || get.Params[i].Default != "full" || !slices.Equal(get.Params[i].Enum, []any{"minimal", "full", "raw", "metadata"}) {
		t.Errorf("get_message format param = %+v", get.Params)
	}
}
