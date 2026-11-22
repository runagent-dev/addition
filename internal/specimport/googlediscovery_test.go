package specimport_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/specimport"
)

func notesSet(t *testing.T) *catalog.OperationSet {
	t.Helper()
	data, err := os.ReadFile("testdata/notes_discovery.json")
	if err != nil {
		t.Fatal(err)
	}
	set, err := specimport.GoogleDiscovery("notes", data)
	if err != nil {
		t.Fatalf("GoogleDiscovery: %v", err)
	}
	return set
}

func opByID(t *testing.T, set *catalog.OperationSet, id string) catalog.Operation {
	t.Helper()
	for _, op := range set.Operations {
		if op.ID == id {
			return op
		}
	}
	t.Fatalf("operation %q not found", id)
	return catalog.Operation{}
}

func TestSourceAndHost(t *testing.T) {
	set := notesSet(t)
	if set.Provider != "notes" || set.Host != "notes.example.test" {
		t.Errorf("provider/host = %q/%q", set.Provider, set.Host)
	}
	want := catalog.OperationSource{Kind: "google-discovery", ID: "notes:v2", Revision: "20260101", Docs: "https://example.test/docs/notes"}
	if set.Source != want {
		t.Errorf("source = %+v, want %+v", set.Source, want)
	}
}

func TestOperationsAreNestedResourcesFlattenedAndSorted(t *testing.T) {
	set := notesSet(t)
	var ids []string
	for _, op := range set.Operations {
		ids = append(ids, op.ID)
	}
	want := []string{"notes.batchDelete", "notes.comments.get", "notes.create", "notes.delete", "notes.get", "notes.list", "notes.patch", "notes.search"}
	if !slices.Equal(ids, want) {
		t.Errorf("ids = %v, want %v", ids, want)
	}
}

func TestListOperation(t *testing.T) {
	op := opByID(t, notesSet(t), "notes.list")
	if op.Method != "GET" || op.Path != "/notes/v2/notes" || op.Effect != catalog.EffectRead {
		t.Errorf("method/path/effect = %s %s %s", op.Method, op.Path, op.Effect)
	}
	if op.Summary != "Lists the notes of the signed-in user." {
		t.Errorf("summary = %q", op.Summary)
	}
	if !slices.Equal(op.Scopes, []string{"https://example.test/auth/notes", "https://example.test/auth/notes.readonly"}) {
		t.Errorf("scopes = %v, want sorted", op.Scopes)
	}
	params := map[string]catalog.OperationParam{}
	for _, p := range op.Params {
		params[p.Name] = p
	}
	if _, ok := params["legacyFlag"]; ok {
		t.Error("deprecated parameter should be dropped")
	}
	if _, ok := params["key"]; ok {
		t.Error("global parameter key should not be listed")
	}
	if p := params["pageSize"]; p.In != "query" || p.Type != "integer" || p.Default != "10" || p.Required {
		t.Errorf("pageSize = %+v", p)
	}
	if p := params["labels"]; p.Type != "string[]" {
		t.Errorf("labels type = %q, want string[]", p.Type)
	}
	if p := params["minScore"]; p.Type != "number" {
		t.Errorf("minScore type = %q, want number", p.Type)
	}
	if p := params["order"]; !slices.Equal(p.Enum, []string{"newest", "oldest"}) {
		t.Errorf("order enum = %v", p.Enum)
	}
	if _, ok := params["fields"]; !ok {
		t.Error("the global fields parameter should be listed")
	}
	if _, ok := params["alt"]; ok {
		t.Error("the global alt parameter should not be listed")
	}
}

func TestPathParametersAreRequiredAndDeclared(t *testing.T) {
	op := opByID(t, notesSet(t), "notes.get")
	if op.Path != "/notes/v2/notes/{noteId}" {
		t.Errorf("path = %q", op.Path)
	}
	if len(op.Params) != 2 || op.Params[0].Name != "noteId" || op.Params[0].In != "path" || !op.Params[0].Required {
		t.Errorf("params = %+v, want path noteId first then fields", op.Params)
	}
}

func TestReservedExpansionPathUsesFlatPlaceholders(t *testing.T) {
	op := opByID(t, notesSet(t), "notes.comments.get")
	if op.Path != "/notes/v2/{name}" {
		t.Errorf("path = %q, want /notes/v2/{name}", op.Path)
	}
	if op.Params[0].Name != "name" || op.Params[0].Pattern != "^notes/[^/]+/comments/[^/]+$" {
		t.Errorf("name param = %+v", op.Params[0])
	}
}

func TestEffects(t *testing.T) {
	set := notesSet(t)
	for id, want := range map[string]catalog.Effect{
		"notes.list":        catalog.EffectRead,
		"notes.get":         catalog.EffectRead,
		"notes.search":      catalog.EffectRead,
		"notes.create":      catalog.EffectWrite,
		"notes.patch":       catalog.EffectWrite,
		"notes.delete":      catalog.EffectDestructive,
		"notes.batchDelete": catalog.EffectDestructive,
	} {
		if got := opByID(t, set, id).Effect; got != want {
			t.Errorf("%s effect = %s, want %s", id, got, want)
		}
	}
}

func TestRequestBodyListsTopLevelFields(t *testing.T) {
	op := opByID(t, notesSet(t), "notes.create")
	if op.Body == nil || op.Body.Schema != "Note" {
		t.Fatalf("body = %+v", op.Body)
	}
	got := map[string]catalog.BodyField{}
	for _, f := range op.Body.Fields {
		got[f.Name] = f
	}
	if got["title"].Type != "string" || got["title"].Description != "The note title." {
		t.Errorf("title = %+v", got["title"])
	}
	if got["body"].Description != "Free text, see formatting for details." {
		t.Errorf("markdown links should collapse to their text: %q", got["body"].Description)
	}
	if got["tag"].Description != "Optional. Output only. A short tag." {
		t.Errorf("a leading label such as Optional. should not stand alone: %q", got["tag"].Description)
	}
	if got["labels"].Type != "string[]" || got["pinned"].Type != "boolean" || got["meta"].Type != "object" {
		t.Errorf("types = %v %v %v", got["labels"].Type, got["pinned"].Type, got["meta"].Type)
	}
	if opByID(t, notesSet(t), "notes.get").Body != nil {
		t.Error("a GET has no body")
	}
}

func TestMediaUploadPath(t *testing.T) {
	if got := opByID(t, notesSet(t), "notes.create").UploadPath; got != "/upload/notes/v2/notes" {
		t.Errorf("upload path = %q", got)
	}
	if got := opByID(t, notesSet(t), "notes.get").UploadPath; got != "" {
		t.Errorf("upload path on get = %q", got)
	}
}

func TestOutputIsDeterministicAndPassesCatalogValidation(t *testing.T) {
	set := notesSet(t)
	a, err := specimport.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := specimport.Marshal(notesSet(t))
	if string(a) != string(b) {
		t.Error("Marshal is not deterministic")
	}
	if !strings.HasPrefix(string(a), "# Generated by tools/specgen") {
		t.Errorf("output should start with the generated-file banner, got %.60q", a)
	}
	if err := catalog.ValidateOperationSet(set, nil); err != nil {
		t.Errorf("generated set fails catalog validation: %v", err)
	}
}

func TestRejectsDocumentsItCannotTrust(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"http root": func(s string) string {
			return strings.Replace(s, "https://notes.example.test/", "http://notes.example.test/", 1)
		},
		"other format": func(s string) string {
			return strings.Replace(s, `"discoveryVersion": "v1"`, `"discoveryVersion": "v9"`, 1)
		},
		"unknown verb": func(s string) string { return strings.Replace(s, `"httpMethod": "PATCH"`, `"httpMethod": "TRACE"`, 1) },
		"root with a path": func(s string) string {
			return strings.Replace(s, "https://notes.example.test/", "https://notes.example.test/x/", 1)
		},
		"not json": func(string) string { return "name: notes" },
	} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/notes_discovery.json")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := specimport.GoogleDiscovery("notes", []byte(mutate(string(data)))); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
