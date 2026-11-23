package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

type searchHit struct {
	Op       string   `json:"op"`
	App      string   `json:"app"`
	Summary  string   `json:"summary"`
	Effect   string   `json:"effect"`
	Method   string   `json:"method"`
	Path     string   `json:"path"`
	Required []string `json:"required"`
	Describe string   `json:"describe"`
}

type searchOut struct {
	Query     string      `json:"query"`
	Total     int         `json:"total"`
	Results   []searchHit `json:"results"`
	Truncated bool        `json:"truncated"`
	Apps      []struct {
		App        string         `json:"app"`
		Operations int            `json:"operations"`
		Effects    map[string]int `json:"effects"`
	} `json:"apps"`
	Next string `json:"next"`
}

func decodeObject[T any](t *testing.T, r result) T {
	t.Helper()
	if r.code != ExitOK {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", r.code, r.stderr)
	}
	var v T
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatalf("stdout is not the expected JSON: %v\n%q", err, r.stdout)
	}
	return v
}

func TestSearchFindsAnOperationAndPointsToDescribe(t *testing.T) {
	out := decodeObject[searchOut](t, run("search", "send", "an", "email"))
	if len(out.Results) == 0 || out.Results[0].Op != "gmail.users.messages.send" {
		t.Fatalf("results = %+v", out.Results)
	}
	hit := out.Results[0]
	if hit.App != "gmail" || hit.Method != "POST" || hit.Effect != "write" || hit.Path != "/gmail/v1/users/{userId}/messages/send" {
		t.Errorf("hit = %+v", hit)
	}
	if hit.Describe != "addition describe gmail.users.messages.send" {
		t.Errorf("describe = %q", hit.Describe)
	}
	if slices.Contains(hit.Required, "userId") {
		t.Errorf("a defaulted path parameter should not be listed as required: %v", hit.Required)
	}
	if len(out.Results) > 8 || out.Query != "send an email" || !strings.Contains(out.Next, "addition describe") {
		t.Errorf("query=%q results=%d next=%q", out.Query, len(out.Results), out.Next)
	}
}

func TestSearchFiltersAndLimit(t *testing.T) {
	out := decodeObject[searchOut](t, run("search", "list", "--app", "google-drive", "--effect", "read", "--limit", "3"))
	if len(out.Results) != 3 || !out.Truncated || out.Total <= 3 {
		t.Fatalf("results=%d truncated=%v total=%d", len(out.Results), out.Truncated, out.Total)
	}
	for _, h := range out.Results {
		if h.App != "google-drive" || h.Effect != "read" {
			t.Errorf("filter leaked %+v", h)
		}
	}
}

func TestSearchWithoutAQueryListsTheApps(t *testing.T) {
	out := decodeObject[searchOut](t, run("search"))
	if len(out.Results) != 0 || len(out.Apps) < 5 {
		t.Fatalf("apps = %+v", out.Apps)
	}
	var gmailOps int
	for _, a := range out.Apps {
		if a.App == "gmail" {
			gmailOps = a.Operations
			if a.Effects["read"] == 0 || a.Effects["destructive"] == 0 {
				t.Errorf("gmail effects = %v", a.Effects)
			}
		}
	}
	if gmailOps < 70 || !strings.Contains(out.Next, "addition search") {
		t.Errorf("gmail ops = %d, next = %q", gmailOps, out.Next)
	}
}

func TestSearchWithNoMatchStillGuides(t *testing.T) {
	out := decodeObject[searchOut](t, run("search", "zzzzqqqq"))
	if len(out.Results) != 0 || out.Total != 0 || len(out.Apps) == 0 || !strings.Contains(out.Next, "addition search") {
		t.Errorf("out = %+v", out)
	}
}

func TestSearchRejectsBadInput(t *testing.T) {
	for _, args := range [][]string{
		{"search", "x", "--limit", "0"},
		{"search", "x", "--limit", "999"},
		{"search", "x", "--effect", "maybe"},
		{"search", "x", "--app", "nope"},
	} {
		r := run(args...)
		if r.code != ExitUsage {
			t.Errorf("%v exit = %d, want %d (stderr %q)", args, r.code, ExitUsage, r.stderr)
		}
	}
}

type describeOut struct {
	Op        string `json:"op"`
	App       string `json:"app"`
	Summary   string `json:"summary"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Effect    string `json:"effect"`
	Scopes    []string `json:"scopes_any_of"`
	Params    []struct {
		Name     string `json:"name"`
		In       string `json:"in"`
		Required bool   `json:"required"`
	} `json:"params"`
	Body *struct {
		Schema string `json:"schema"`
	} `json:"body"`
	Call string `json:"call"`
	Docs string `json:"docs"`
}

func TestDescribeShowsEverythingNeededToCall(t *testing.T) {
	out := decodeObject[describeOut](t, run("describe", "gmail.users.messages.send"))
	if out.App != "gmail" || out.Method != "POST" || out.Effect != "write" || out.Summary == "" {
		t.Fatalf("out = %+v", out)
	}
	if !slices.Contains(out.Scopes, "https://www.googleapis.com/auth/gmail.send") {
		t.Errorf("scopes = %v", out.Scopes)
	}
	if out.Body == nil || out.Body.Schema != "Message" {
		t.Errorf("body = %+v", out.Body)
	}
	want := `addition call gmail -a <alias> POST "/gmail/v1/users/me/messages/send" --data '<json>'`
	if out.Call != want {
		t.Errorf("call = %q, want %q", out.Call, want)
	}
	if out.Docs == "" {
		t.Error("docs link missing")
	}
}

func TestDescribeCallTemplateUsesPlaceholdersAndRequiredQuery(t *testing.T) {
	out := decodeObject[describeOut](t, run("describe", "drive.files.get"))
	if !strings.Contains(out.Call, `"/drive/v3/files/$FILE_ID"`) || !strings.HasPrefix(out.Call, "addition call google-drive -a <alias> GET ") {
		t.Errorf("call = %q", out.Call)
	}
	if strings.Contains(out.Call, "--data") {
		t.Errorf("a GET has no --data: %q", out.Call)
	}
}

func TestDescribeUnknownOperationNamesTheNearestOnes(t *testing.T) {
	r := run("describe", "gmail.users.messages.sned")
	if r.code != ExitUsage {
		t.Fatalf("exit = %d, stderr %q", r.code, r.stderr)
	}
	env := decodeError(t, r.stderr)
	if env.Error.Code != "unknown-operation" || !strings.Contains(env.Error.Message, "gmail.users.messages.send") || !strings.Contains(env.Error.Message, "addition search") {
		t.Errorf("error = %+v", env.Error)
	}
}

func TestSearchAndDescribeNeedNoVaultOrNetwork(t *testing.T) {
	t.Setenv("ADDITION_HOME", t.TempDir()+"/does-not-exist")
	t.Setenv("ADDITION_MASTER_KEY", "")
	if r := run("search", "calendar events"); r.code != ExitOK {
		t.Errorf("search exit = %d, stderr %q", r.code, r.stderr)
	}
	if r := run("describe", "calendar.events.list"); r.code != ExitOK {
		t.Errorf("describe exit = %d, stderr %q", r.code, r.stderr)
	}
}
