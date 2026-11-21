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

func TestAppsTopicListsEveryConnectableApp(t *testing.T) {
	c := shippedCatalog(t)
	g, err := Load(shared.Guide, c)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	apps, _ := g.Topic("apps")
	for _, p := range c.Providers() {
		listed := strings.Contains(apps.Body, "`"+p.ID+"`")
		if c.Connectable(p.ID) != listed {
			t.Errorf("%s: connectable=%v but listed=%v", p.ID, c.Connectable(p.ID), listed)
		}
	}
	for _, want := range []string{"drive", "www.googleapis.com", "gmail.readonly", "addition guide gmail"} {
		if !strings.Contains(apps.Body, want) {
			t.Errorf("apps topic lacks %q:\n%s", want, apps.Body)
		}
	}
	if strings.Contains(apps.Body, "https://www.googleapis.com/auth/") {
		t.Error("scopes are not shortened")
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
