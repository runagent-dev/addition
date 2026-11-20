package cli

import (
	"encoding/json"
	"slices"
	"testing"
)

type providerRow struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	AuthType     string  `json:"auth_type"`
	Extends      *string `json:"extends"`
	IdentityRoot string  `json:"identity_root"`
}

type toolRow struct {
	Provider    string `json:"provider"`
	Command     string `json:"command"`
	Tool        string `json:"tool"`
	CLIName     string `json:"cli_name"`
	Description string `json:"description"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Effect      string `json:"effect"`
}

func decodeRows[T any](t *testing.T, r result) []T {
	t.Helper()
	if r.code != ExitOK {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", r.code, r.stderr)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want empty", r.stderr)
	}
	var rows []T
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\nstdout: %q", err, r.stdout)
	}
	return rows
}

func TestProvidersListsCatalogSortedByID(t *testing.T) {
	rows := decodeRows[providerRow](t, run("providers"))

	var ids []string
	byID := map[string]providerRow{}
	for _, p := range rows {
		ids = append(ids, p.ID)
		byID[p.ID] = p
	}
	if want := []string{"github", "gmail", "google", "google-calendar", "google-drive", "slack"}; !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}

	drive := byID["google-drive"]
	if drive.Name != "Google Drive" || drive.AuthType != "oauth2" || drive.IdentityRoot != "google" {
		t.Errorf("google-drive row = %+v", drive)
	}
	if drive.Extends == nil || *drive.Extends != "google" {
		t.Errorf("google-drive extends = %v, want google", drive.Extends)
	}
	github := byID["github"]
	if github.Extends != nil || github.IdentityRoot != "github" {
		t.Errorf("github row = %+v, want extends null and root github", github)
	}
}

func TestProvidersWorksFromAnyDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	if rows := decodeRows[providerRow](t, run("providers")); len(rows) == 0 {
		t.Error("no providers listed outside the repo")
	}
}

func TestProvidersRejectsArguments(t *testing.T) {
	r := run("providers", "extra")
	if r.code != ExitUsage || decodeError(t, r.stderr).Error.Code != "usage" {
		t.Errorf("exit=%d stderr=%q, want usage error", r.code, r.stderr)
	}
}

func TestToolsListsEveryTool(t *testing.T) {
	rows := decodeRows[toolRow](t, run("tools"))

	providers := map[string]bool{}
	for _, row := range rows {
		providers[row.Provider] = true
		if row.Tool == "" || row.CLIName == "" || row.Method == "" || row.Path == "" || row.Effect == "" || row.Description == "" {
			t.Errorf("incomplete row: %+v", row)
		}
	}
	for _, p := range []string{"github", "gmail", "google-calendar", "google-drive", "slack"} {
		if !providers[p] {
			t.Errorf("no tools listed for %s", p)
		}
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Provider > rows[i].Provider {
			t.Fatalf("rows not grouped by provider id: %q before %q", rows[i-1].Provider, rows[i].Provider)
		}
	}
}

func TestToolsFiltersByProvider(t *testing.T) {
	rows := decodeRows[toolRow](t, run("tools", "--provider", "gmail"))

	if len(rows) != 5 {
		t.Fatalf("got %d gmail tools, want 5", len(rows))
	}
	for _, row := range rows {
		if row.Provider != "gmail" || row.Command != "gmail" {
			t.Errorf("row from wrong toolkit: %+v", row)
		}
	}
	want := toolRow{
		Provider: "gmail", Command: "gmail", Tool: "search_messages", CLIName: "search-messages",
		Method: "GET", Path: "/gmail/v1/users/me/messages", Effect: "read",
	}
	i := slices.IndexFunc(rows, func(r toolRow) bool { return r.Tool == "search_messages" })
	if i < 0 {
		t.Fatal("search_messages missing")
	}
	got := rows[i]
	got.Description = ""
	if got != want {
		t.Errorf("search_messages row = %+v, want %+v", got, want)
	}
}

func TestToolsUsesToolkitCommandName(t *testing.T) {
	for provider, command := range map[string]string{"google-drive": "drive", "google-calendar": "calendar"} {
		rows := decodeRows[toolRow](t, run("tools", "--provider", provider))
		if len(rows) == 0 {
			t.Fatalf("no tools for %s", provider)
		}
		for _, row := range rows {
			if row.Command != command {
				t.Errorf("%s/%s command = %q, want %q", provider, row.Tool, row.Command, command)
			}
		}
	}
	rows := decodeRows[toolRow](t, run("tools", "--provider", "google-drive"))
	i := slices.IndexFunc(rows, func(r toolRow) bool { return r.Tool == "trash_file" })
	if i < 0 || rows[i].Effect != "destructive" || rows[i].CLIName != "trash-file" {
		t.Errorf("trash_file row missing or wrong: %+v", rows)
	}
}

func TestToolsForProviderWithoutToolkitIsEmptyArray(t *testing.T) {
	r := run("tools", "--provider", "google")
	if rows := decodeRows[toolRow](t, r); len(rows) != 0 {
		t.Errorf("rows = %+v, want none", rows)
	}
	if r.stdout != "[]\n" {
		t.Errorf("stdout = %q, want an empty JSON array", r.stdout)
	}
}

func TestToolsUnknownProviderIsUsageError(t *testing.T) {
	r := run("tools", "--provider", "nope")

	if r.code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", r.code, ExitUsage)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q, want empty", r.stdout)
	}
	env := decodeError(t, r.stderr)
	if env.Error.Code != "usage" || env.Error.Message == "" {
		t.Errorf("envelope = %+v", env)
	}
}
