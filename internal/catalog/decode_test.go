package catalog

import (
	"slices"
	"strings"
	"testing"
)

func pingTool(description, paramsLine string) string {
	return `  - id: ping
    name: "Ping"
    description: ` + description + `
    operation: "GET /ping"
    effect: read
` + paramsLine
}

func TestMappingKeysMustBePlainStrings(t *testing.T) {
	cases := []struct {
		name      string
		toolkit   string
		fragments []string
	}{
		{"alias key hides a missing params", acmeToolkit(pingTool("&params paramz", "    *params : []\n")),
			[]string{"toolkits/acme.yaml", "line 10", "key"}},
		{"alias key resolving to params", acmeToolkit(pingTool("&k params", "    *k : []\n")),
			[]string{"toolkits/acme.yaml", "line 10", "key"}},
		{"anchored key", acmeToolkit(pingTool(`"Ping"`, "    &p params: []\n")),
			[]string{"toolkits/acme.yaml", "line 10", "key"}},
		{"alias key in response fields", acmeToolkit(pingTool(`"Ping"`,
			"    params: []\n    response:\n      fields: {&k id: first, *k : second}\n")),
			[]string{"toolkits/acme.yaml", "line 12", "key"}},
		{"non-string key", acmeToolkit(pingTool(`"Ping"`, "    params: []\n    response:\n      fields: {1: first}\n")),
			[]string{"toolkits/acme.yaml", "line 12", "key"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantLoadError(t, toolkitFS(map[string]string{"acme.yaml": tc.toolkit}), tc.fragments...)
		})
	}

	t.Run("alias key in a provider", func(t *testing.T) {
		provider := strings.Replace(baseProvider, `name: "Base"`, "name: &n Base\n*n : x", 1)
		wantLoadError(t, providerFS(map[string]string{"base.yaml": provider}), "providers/base.yaml", "line 3", "key")
	})

	t.Run("value aliases still load", func(t *testing.T) {
		mustLoad(t, toolkitFS(map[string]string{"acme.yaml": acmeToolkit(pingTool("&d Ping",
			"    params:\n      - name: q\n        type: string\n        required: false\n        description: *d\n"))}))
	})
}

func TestFilesHoldExactlyOneDocument(t *testing.T) {
	t.Run("provider", func(t *testing.T) {
		wantLoadError(t, providerFS(map[string]string{"base.yaml": baseProvider + "---\nid: other\n"}),
			"providers/base.yaml", "one document")
	})
	t.Run("toolkit", func(t *testing.T) {
		wantLoadError(t, toolkitFS(map[string]string{"acme.yaml": acmeToolkit(validTool) + "---\nprovider: acme\n"}),
			"toolkits/acme.yaml", "one document")
	})
	t.Run("trailing syntax error", func(t *testing.T) {
		wantLoadError(t, toolkitFS(map[string]string{"acme.yaml": acmeToolkit(validTool) + "---\n[unclosed\n"}),
			"toolkits/acme.yaml")
	})
}

func TestScalarsResolveAsYAML12Core(t *testing.T) {
	tool := `  - id: list_widgets
    name: 2026-02-01
    description: 2026-02-02
    operation: "GET /widgets"
    effect: read
    params:
      - name: n
        type: integer
        required: false
        default: 010
        enum: [010, 08, 20]
        description: "Page size"
      - name: since
        type: string
        required: false
        default: 2026-02-01
        enum: [2026-02-01, 2026-03-01]
        description: "Day"
`
	c := mustLoad(t, toolkitFS(map[string]string{"acme.yaml": acmeToolkit(tool)}))
	tk, _ := c.Toolkit("acme")
	got := tk.Tools[0]
	if got.Name != "2026-02-01" || got.Description != "2026-02-02" {
		t.Errorf("name=%q description=%q, want literal dates", got.Name, got.Description)
	}
	n, since := got.Params[0], got.Params[1]
	if n.Default != 10 || !slices.Equal(n.Enum, []any{10, 8, 20}) {
		t.Errorf("n default=%#v enum=%#v, want base-10 integers", n.Default, n.Enum)
	}
	if since.Default != "2026-02-01" || !slices.Equal(since.Enum, []any{"2026-02-01", "2026-03-01"}) {
		t.Errorf("since default=%#v enum=%#v, want literal strings", since.Default, since.Enum)
	}
}

func TestProjectedCommandNamesAreValid(t *testing.T) {
	swap := func(old, new string) string {
		return acmeToolkit(strings.Replace(validTool, old, new, 1))
	}
	cases := []struct {
		name      string
		toolkit   string
		fragments []string
	}{
		{"tool id with a space", acmeToolkit(validTool + strings.Replace(validTool, "id: get_widget", `id: "get widget"`, 1)),
			[]string{"toolkits/acme.yaml", "get widget", "command name"}},
		{"tool id with a double underscore", swap("id: get_widget", "id: get__widget"), []string{"get--widget", "command name"}},
		{"tool id with a trailing underscore", swap("id: get_widget", "id: get_"), []string{"get-", "command name"}},
		{"tool id with uppercase", swap("id: get_widget", "id: getWidget"), []string{"getWidget", "command name"}},
		{"toolkit command with a trailing dash", strings.Replace(acmeToolkit(validTool), "name: Acme", "command: acme-\nname: Acme", 1),
			[]string{"toolkits/acme.yaml", "acme-"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantLoadError(t, toolkitFS(map[string]string{"acme.yaml": tc.toolkit}), tc.fragments...)
		})
	}
}

func TestOptionalPlaceholderDefaults(t *testing.T) {
	placeholder := func(typ, def string) string {
		return acmeToolkit(strings.Replace(validTool, "        type: string\n        required: true\n",
			"        type: "+typ+"\n        required: false\n        default: "+def+"\n", 1))
	}
	t.Run("empty string default is rejected", func(t *testing.T) {
		wantLoadError(t, toolkitFS(map[string]string{"acme.yaml": placeholder("string", `""`)}),
			`tool "get_widget"`, "widgetId", "default")
	})
	t.Run("zero integer default loads", func(t *testing.T) {
		mustLoad(t, toolkitFS(map[string]string{"acme.yaml": placeholder("integer", "0")}))
	})
}

func TestNullResponseFieldsLoadAsOmitted(t *testing.T) {
	tool := strings.Replace(validTool, "effect: read", "effect: read\n    response:\n      type: object\n      fields: null", 1)
	c := mustLoad(t, toolkitFS(map[string]string{"acme.yaml": acmeToolkit(tool)}))
	tk, _ := c.Toolkit("acme")
	if r := tk.Tools[0].Response; r == nil || r.Fields != nil {
		t.Errorf("response = %+v, want fields unset", r)
	}

	nested := strings.Replace(validTool, "effect: read", "effect: read\n    response:\n      fields: {data: {title: t}}", 1)
	wantLoadError(t, toolkitFS(map[string]string{"acme.yaml": acmeToolkit(nested)}), `tool "get_widget"`, "fields")
}
