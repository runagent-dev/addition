package guide

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/shared"
)

func shippedCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Load(shared.FS)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return c
}

func miniGuide(extra map[string]string) fstest.MapFS {
	files := fstest.MapFS{}
	for _, id := range coreTopics {
		if id != "apps" {
			files[id+".md"] = &fstest.MapFile{Data: []byte("Summary of " + id + ".\n\n# " + id + "\n")}
		}
	}
	for name, body := range extra {
		files[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return files
}

func TestShippedGuideLoads(t *testing.T) {
	g, err := Load(shared.Guide, shippedCatalog(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, id := range []string{"overview", "connect", "call", "errors", "accounts", "apps", "troubleshooting", "gmail", "google-drive", "google-calendar", "google-chat", "google-docs"} {
		topic, ok := g.Topic(id)
		if !ok {
			t.Errorf("topic %q is missing", id)
			continue
		}
		if topic.Summary == "" || !strings.HasPrefix(topic.Body, topic.Summary) {
			t.Errorf("topic %q: summary %q is not the first line of its body", id, topic.Summary)
		}
	}
}

func TestSummaryIsTheFirstLine(t *testing.T) {
	g, err := Load(miniGuide(nil), shippedCatalog(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	topic, _ := g.Topic("connect")
	if topic.Summary != "Summary of connect." {
		t.Errorf("summary = %q", topic.Summary)
	}
}

func TestLoadRejectsBrokenGuides(t *testing.T) {
	cases := map[string]map[string]string{
		"blank summary":          {"connect.md": "\n# connect\n"},
		"heading as summary":     {"connect.md": "# connect\n"},
		"recipe named like all":  {"apps/all.md": "Recipe.\n"},
		"recipe named like core": {"apps/call.md": "Recipe.\n"},
		"unknown provider":       {"apps/not-a-provider.md": "Recipe.\n"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(miniGuide(extra), shippedCatalog(t)); err == nil {
				t.Fatal("Load accepted the guide")
			}
		})
	}
}

func TestLoadRejectsMissingCoreTopic(t *testing.T) {
	files := miniGuide(nil)
	delete(files, "errors.md")
	if _, err := Load(files, shippedCatalog(t)); err == nil {
		t.Fatal("Load accepted a guide without errors.md")
	}
}

func tableRows(body string) []string {
	var rows []string
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "| `") {
			rows = append(rows, line)
		}
	}
	return rows
}

func TestAppsTopicListsOnlyTheGoogleApps(t *testing.T) {
	c := shippedCatalog(t)
	g, err := Load(shared.Guide, c)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	apps, _ := g.Topic("apps")
	table := strings.Join(tableRows(apps.Body), "\n")
	for _, p := range c.Providers() {
		listed := strings.Contains(table, "| `"+p.ID+"` |")
		want := p.IdentityRoot == "google" && c.Connectable(p.ID)
		if listed != want {
			t.Errorf("%s: in the supported table = %v, want %v", p.ID, listed, want)
		}
	}
	for _, want := range []string{"drive", "www.googleapis.com", "gmail.readonly", "Recipe `addition guide <id>`"} {
		if !strings.Contains(apps.Body, want) {
			t.Errorf("apps topic lacks %q:\n%s", want, apps.Body)
		}
	}
	if strings.Contains(apps.Body, "https://www.googleapis.com/auth/") {
		t.Error("scopes are not shortened")
	}
}

func TestAppsTopicMentionsOtherProvidersOnOneUnsupportedLine(t *testing.T) {
	g, _ := Load(shared.Guide, shippedCatalog(t))
	apps, _ := g.Topic("apps")
	var unsupported, tokens []string
	for line := range strings.SplitSeq(apps.Body, "\n") {
		if strings.Contains(line, "`slack`") {
			unsupported = append(unsupported, line)
		}
		if strings.Contains(line, "`github`") {
			tokens = append(tokens, line)
		}
	}
	if len(unsupported) != 1 || !strings.Contains(unsupported[0], "not supported in this release") || strings.Contains(unsupported[0], "`github`") {
		t.Errorf("slack must be named on one line saying it is not supported in this release, got %q", unsupported)
	}
	if len(tokens) != 1 || !strings.Contains(tokens[0], "--token-stdin") || strings.Contains(tokens[0], "not supported") {
		t.Errorf("github must be named on one line that says it connects with a token, got %q", tokens)
	}
	if strings.Contains(apps.Body, "Apps this binary can connect") || strings.Contains(apps.Body, "use `addition call`") {
		t.Error("the apps topic still advertises every catalog entry")
	}
}

func TestIndexIsOverviewThenTopics(t *testing.T) {
	g, _ := Load(shared.Guide, shippedCatalog(t))
	overview, _ := g.Topic("overview")
	index := g.Index("9.9.9")
	if !strings.HasPrefix(index, overview.Body) {
		t.Error("index does not start with the overview")
	}
	if !strings.Contains(index, "## Topics") || !strings.Contains(index, "9.9.9") {
		t.Errorf("index lacks the topic list or the version:\n%s", index)
	}
	for _, e := range g.Entries() {
		if !strings.Contains(index, e.Command) {
			t.Errorf("index does not list %q", e.Command)
		}
	}
}

func TestEntriesEndWithAll(t *testing.T) {
	g, _ := Load(shared.Guide, shippedCatalog(t))
	entries := g.Entries()
	last := entries[len(entries)-1]
	if last.ID != "all" || last.Command != "addition guide all" {
		t.Errorf("last entry = %+v", last)
	}
}
