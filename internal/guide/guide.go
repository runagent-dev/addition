// Package guide assembles the agent-facing manual from Markdown embedded in the binary.
package guide

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/runagent-dev/addition/internal/catalog"
)

const (
	appsTopic   = "apps"
	allTopic    = "all"
	recipeDir   = "apps"
	topicSuffix = ".md"
)

// coreTopics are the topics every guide has, in reading order; apps is generated and the rest are files.
var coreTopics = []string{"overview", "discover", "connect", "call", "accounts", "errors", "troubleshooting", appsTopic}

// Topic is one page of the guide. Its Summary is the first line of its Body.
type Topic struct {
	ID      string
	Summary string
	Body    string
}

// Entry is a topic as listed for discovery.
type Entry struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
	Command string `json:"command"`
}

// Guide is the loaded manual.
type Guide struct {
	topics []Topic
}

// Load reads every topic from fsys and generates the apps topic from c.
func Load(fsys fs.FS, c *catalog.Catalog) (*Guide, error) {
	files := map[string]Topic{}
	for _, id := range coreTopics {
		if id == appsTopic {
			continue
		}
		topic, err := readTopic(fsys, id+topicSuffix, id)
		if err != nil {
			return nil, err
		}
		files[id] = topic
	}
	recipes, err := loadRecipes(fsys, c)
	if err != nil {
		return nil, err
	}
	apps, err := newTopic(appsTopic, renderApps(c, recipes))
	if err != nil {
		return nil, err
	}
	files[appsTopic] = apps

	g := &Guide{}
	for _, id := range coreTopics {
		g.topics = append(g.topics, files[id])
	}
	g.topics = append(g.topics, recipes...)
	return g, nil
}

func loadRecipes(fsys fs.FS, c *catalog.Catalog) ([]Topic, error) {
	names, err := fs.ReadDir(fsys, recipeDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recipes []Topic
	for _, entry := range names {
		id, isMarkdown := strings.CutSuffix(entry.Name(), topicSuffix)
		if !isMarkdown {
			return nil, fmt.Errorf("guide/%s/%s is not a Markdown recipe", recipeDir, entry.Name())
		}
		if id == allTopic || slices.Contains(coreTopics, id) {
			return nil, fmt.Errorf("recipe %q has the name of a core topic", id)
		}
		if _, ok := c.Provider(id); !ok || !c.Connectable(id) {
			return nil, fmt.Errorf("recipe %q is not a connectable provider id", id)
		}
		topic, err := readTopic(fsys, path.Join(recipeDir, entry.Name()), id)
		if err != nil {
			return nil, err
		}
		recipes = append(recipes, topic)
	}
	return recipes, nil
}

func readTopic(fsys fs.FS, name, id string) (Topic, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return Topic{}, fmt.Errorf("guide topic %q: %w", id, err)
	}
	return newTopic(id, string(data))
}

func newTopic(id, body string) (Topic, error) {
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	summary, _, _ := strings.Cut(body, "\n")
	if strings.TrimSpace(summary) == "" || strings.HasPrefix(summary, "#") {
		return Topic{}, fmt.Errorf("guide topic %q must start with a one-line summary, not %q", id, summary)
	}
	return Topic{ID: id, Summary: summary, Body: body}, nil
}

// Topics lists the topics in reading order, without the synthetic all.
func (g *Guide) Topics() []Topic { return g.topics }

// Topic finds one topic by id.
func (g *Guide) Topic(id string) (Topic, bool) {
	for _, t := range g.topics {
		if t.ID == id {
			return t, true
		}
	}
	return Topic{}, false
}

// Entries lists every topic for discovery, ending with all.
func (g *Guide) Entries() []Entry {
	entries := make([]Entry, 0, len(g.topics)+1)
	for _, t := range g.topics {
		entries = append(entries, entryFor(t.ID, t.Summary))
	}
	return append(entries, entryFor(allTopic, "Every topic in one document, to load the whole manual in a single call"))
}

func entryFor(id, summary string) Entry {
	return Entry{ID: id, Summary: summary, Command: "addition guide " + id}
}

// Index is the overview followed by the list of topics.
func (g *Guide) Index(version string) string {
	var b strings.Builder
	overview, _ := g.Topic("overview")
	b.WriteString(overview.Body)
	b.WriteString("\n## Topics\n\n")
	for _, e := range g.Entries() {
		fmt.Fprintf(&b, "- `%s` — %s\n", e.Command, e.Summary)
	}
	fmt.Fprintf(&b, "\nThis manual describes addition %s and is read from the binary itself: no network, no vault.\n", version)
	return b.String()
}

// All is every topic in reading order, separated by rules.
func (g *Guide) All() string {
	bodies := make([]string, len(g.topics))
	for i, t := range g.topics {
		bodies[i] = t.Body
	}
	return strings.Join(bodies, "\n---\n\n")
}

const googleScopePrefix = "https://www.googleapis.com/auth/"

// identityScopes are requested by every app only to learn who signed in.
var identityScopes = []string{"openid", "userinfo.email"}

// supportedRoot is the identity root whose apps this release supports.
const supportedRoot = "google"

func renderApps(c *catalog.Catalog, recipes []Topic) string {
	var b strings.Builder
	b.WriteString("Apps supported in this release: the Google apps, generated from this binary's catalog.\n\n# Apps\n\n")
	b.WriteString("Connect one with `addition connect <app> --as <alias>`; `<app>` is the id or the command name. ")
	b.WriteString("Scopes are shown without the Google `…/auth/` prefix; `openid` and `userinfo.email` are also requested, to learn who signed in.\n\n")
	b.WriteString("| App id | Command name | API host | Scopes | Recipe `addition guide <id>` |\n|---|---|---|---|---|\n")
	for _, p := range c.Apps(supportedRoot) {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s | %s |\n", p.ID, commandName(c, p), apiHost(p), scopeSummary(p), recipeLink(recipes, p.ID))
	}
	if others := unsupportedApps(c); len(others) > 0 {
		fmt.Fprintf(&b, "\nOther catalog entries (%s) are not supported in this release.\n", strings.Join(others, ", "))
	}
	return b.String()
}

func unsupportedApps(c *catalog.Catalog) []string {
	var ids []string
	for _, p := range c.Providers() {
		if p.IdentityRoot != supportedRoot && c.Connectable(p.ID) {
			ids = append(ids, "`"+p.ID+"`")
		}
	}
	return ids
}

func commandName(c *catalog.Catalog, p *catalog.Provider) string {
	if tk, ok := c.Toolkit(p.ID); ok {
		return tk.Command
	}
	return p.ID
}

func apiHost(p *catalog.Provider) string {
	u, err := url.Parse(p.API.BaseURL)
	if err != nil || u.Host == "" {
		return p.API.BaseURL
	}
	return u.Host
}

func scopeSummary(p *catalog.Provider) string {
	var scopes []string
	for _, s := range p.OAuth2.DefaultScopes {
		s = strings.TrimPrefix(s, googleScopePrefix)
		if !slices.Contains(identityScopes, s) {
			scopes = append(scopes, s)
		}
	}
	return strings.Join(scopes, ", ")
}

func recipeLink(recipes []Topic, id string) string {
	if slices.ContainsFunc(recipes, func(t Topic) bool { return t.ID == id }) {
		return "yes"
	}
	return "no"
}
