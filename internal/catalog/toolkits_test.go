package catalog

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

const acmeProvider = `id: "acme"
name: "Acme"
auth_type: "oauth2"
` + oauth2Block + apiBlock

const validTool = `  - id: get_widget
    name: "Get widget"
    description: "Fetch one widget"
    operation: "GET /widgets/{widgetId}"
    effect: read
    params:
      - name: widgetId
        type: string
        required: true
        description: "Widget ID"
`

func toolkitFS(toolkits map[string]string) fstest.MapFS {
	fsys := providerFS(map[string]string{"acme.yaml": acmeProvider})
	for name, body := range toolkits {
		fsys["toolkits/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func acmeToolkit(tools string) string {
	return "provider: acme\nname: Acme\ndescription: Acme widgets\ntools:\n" + tools
}

func TestToolkitLoadsValidTool(t *testing.T) {
	c := mustLoad(t, toolkitFS(map[string]string{
		"acme.yaml":      acmeToolkit(validTool),
		"_template.yaml": "provider: \"\"\n",
	}))

	if got := len(c.Toolkits()); got != 1 {
		t.Fatalf("loaded %d toolkits, want 1", got)
	}
	tk, ok := c.Toolkit("acme")
	if !ok {
		t.Fatal("acme toolkit not loaded")
	}
	if tk.Command != "acme" {
		t.Errorf("command = %q, want the provider id", tk.Command)
	}
	tool := tk.Tools[0]
	if tool.Method != "GET" || tool.Path != "/widgets/{widgetId}" {
		t.Errorf("method=%q path=%q", tool.Method, tool.Path)
	}
	if tool.Effect != EffectRead {
		t.Errorf("effect = %q", tool.Effect)
	}
	if tool.Transform != "" || tool.Pagination != nil {
		t.Errorf("transform=%q pagination=%+v, want unset", tool.Transform, tool.Pagination)
	}
	p := tool.Params[0]
	if p.Name != "widgetId" || p.Type != "string" || !p.Required {
		t.Errorf("param = %+v", p)
	}
}

func TestToolkitCommandOverride(t *testing.T) {
	c := mustLoad(t, toolkitFS(map[string]string{
		"acme.yaml": "provider: acme\ncommand: widgets\nname: Acme\ndescription: d\ntools:\n" + validTool,
	}))
	tk, _ := c.Toolkit("acme")
	if tk.Command != "widgets" {
		t.Errorf("command = %q, want widgets", tk.Command)
	}
}

func TestToolCLIName(t *testing.T) {
	cases := map[string]string{"list_messages": "list-messages", "get": "get", "a_b_c": "a-b-c"}
	for id, want := range cases {
		if got := (Tool{ID: id}).CLIName(); got != want {
			t.Errorf("CLIName(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestToolkitParamDetails(t *testing.T) {
	tools := `  - id: list_widgets
    name: "List"
    description: "List widgets"
    operation: "GET /widgets"
    effect: read
    transform: "acme:list_body"
    params:
      - name: n
        type: integer
        required: false
        default: 10
        enum: [10, 20]
        description: "Page size"
      - name: tags
        type: string[]
        required: false
        description: "Tags"
    pagination:
      type: cursor
      cursor_param: pageToken
      cursor_response_field: meta.next
    response:
      type: array
      fields:
        id: "Widget ID"
`
	c := mustLoad(t, toolkitFS(map[string]string{"acme.yaml": acmeToolkit(tools)}))
	tk, _ := c.Toolkit("acme")
	tool := tk.Tools[0]

	if tool.Transform != "acme:list_body" {
		t.Errorf("transform = %q", tool.Transform)
	}
	n := tool.Params[0]
	if n.Default != 10 || !slices.Equal(n.Enum, []any{10, 20}) {
		t.Errorf("n default=%v enum=%v", n.Default, n.Enum)
	}
	if tool.Params[1].Type != "string[]" {
		t.Errorf("tags type = %q", tool.Params[1].Type)
	}
	if tool.Pagination == nil || tool.Pagination.CursorParam != "pageToken" || tool.Pagination.CursorResponseField != "meta.next" {
		t.Errorf("pagination = %+v", tool.Pagination)
	}
	if tool.Response == nil || tool.Response.Type != "array" || tool.Response.Fields["id"] != "Widget ID" {
		t.Errorf("response = %+v", tool.Response)
	}
}

func TestToolkitValidationErrors(t *testing.T) {
	swap := func(old, new string) string {
		return acmeToolkit(strings.Replace(validTool, old, new, 1))
	}
	param := func(typ string) string {
		return acmeToolkit(validTool + `  - id: bad_param
    name: "Bad"
    description: "d"
    operation: "GET /x"
    effect: read
    params:
      - name: q
        ` + typ + `
        required: true
        description: "d"
`)
	}

	cases := []struct {
		name      string
		toolkit   string
		fragments []string
	}{
		{"unknown provider", strings.Replace(acmeToolkit(validTool), "provider: acme", "provider: phantom", 1),
			[]string{"acme.yaml", "phantom"}},
		{"missing provider field", "name: A\ndescription: d\ntools: []\n", []string{"acme.yaml", "provider"}},
		{"tools not a list", "provider: acme\nname: A\ndescription: d\ntools: {}\n", []string{"acme.yaml"}},
		{"duplicate tool id", acmeToolkit(validTool + validTool), []string{"acme.yaml", "get_widget", "duplicate"}},
		{"missing tool id", swap("id: get_widget", "idd: get_widget"), []string{"acme.yaml", "tool #1", "id"}},
		{"missing operation", swap(`operation: "GET /widgets/{widgetId}"`, ""), []string{"acme.yaml", "get_widget", "operation"}},
		{"operation without path", swap("GET /widgets/{widgetId}", "GET"), []string{"acme.yaml", "get_widget", "METHOD /path"}},
		{"operation with unknown method", swap("GET /widgets/{widgetId}", "FETCH /widgets/{widgetId}"), []string{"acme.yaml", "get_widget", "METHOD /path"}},
		{"operation with lowercase method", swap("GET /widgets/{widgetId}", "get /widgets/{widgetId}"), []string{"get_widget", "METHOD /path"}},
		{"operation path without slash", swap("GET /widgets/{widgetId}", "GET widgets/{widgetId}"), []string{"get_widget", "METHOD /path"}},
		{"operation with trailing junk", swap("GET /widgets/{widgetId}", "GET /widgets/{widgetId} extra"), []string{"get_widget", "METHOD /path"}},
		{"undeclared placeholder", swap("GET /widgets/{widgetId}", "GET /widgets/{widgetId}/parts/{partId}"), []string{"acme.yaml", "get_widget", "partId"}},
		{"missing effect", swap("    effect: read\n", ""), []string{"acme.yaml", "get_widget", "effect"}},
		{"invalid effect", swap("effect: read", "effect: dangerous"), []string{"acme.yaml", "get_widget", "effect", "dangerous"}},
		{"unknown param type", param("type: blobstring"), []string{"acme.yaml", "bad_param", "blobstring"}},
		{"missing param type", param(""), []string{"acme.yaml", "bad_param", "type"}},
		{"param required not set", swap("        required: true\n", ""), []string{"get_widget", "required"}},
		{"param missing description", swap(`        description: "Widget ID"`+"\n", ""), []string{"get_widget", "description"}},
		{"enum of wrong type", param("type: integer\n        enum: [\"one\"]"), []string{"bad_param", "enum"}},
		{"enum on boolean", param("type: boolean\n        enum: [true, false]"), []string{"bad_param", "enum"}},
		{"malformed transform", swap("effect: read", "effect: read\n    transform: no_colon"), []string{"get_widget", "transform"}},
		{"bad pagination type", swap("effect: read", "effect: read\n    pagination:\n      type: scroll\n      cursor_param: p"), []string{"get_widget", "pagination"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantLoadError(t, toolkitFS(map[string]string{"acme.yaml": tc.toolkit}), tc.fragments...)
		})
	}
}

func TestToolkitCollisions(t *testing.T) {
	t.Run("two toolkits for one provider", func(t *testing.T) {
		wantLoadError(t, toolkitFS(map[string]string{
			"a.yaml": acmeToolkit(validTool),
			"b.yaml": acmeToolkit(validTool),
		}), "acme", "duplicate")
	})
	t.Run("two toolkits with one command", func(t *testing.T) {
		fsys := toolkitFS(map[string]string{
			"acme.yaml":  acmeToolkit(validTool),
			"other.yaml": "provider: other\ncommand: acme\nname: O\ndescription: d\ntools:\n" + validTool,
		})
		fsys["providers/other.yaml"] = &fstest.MapFile{Data: []byte(strings.Replace(acmeProvider, `id: "acme"`, `id: "other"`, 1))}
		wantLoadError(t, fsys, "other.yaml", "command", "acme")
	})
}

func TestToolsAreAcceptedWithEveryEffect(t *testing.T) {
	for _, effect := range []Effect{EffectRead, EffectWrite, EffectDestructive} {
		c := mustLoad(t, toolkitFS(map[string]string{
			"acme.yaml": acmeToolkit(strings.Replace(validTool, "effect: read", "effect: "+string(effect), 1)),
		}))
		tk, _ := c.Toolkit("acme")
		if tk.Tools[0].Effect != effect {
			t.Errorf("effect = %q, want %q", tk.Tools[0].Effect, effect)
		}
	}
}

func TestToolkitStrictDecoding(t *testing.T) {
	swap := func(old, new string) string {
		return acmeToolkit(strings.Replace(validTool, old, new, 1))
	}
	parameterless := func(paramsLine string) string {
		return acmeToolkit(`  - id: ping
    name: "Ping"
    description: "Ping"
    operation: "GET /ping"
    effect: read
` + paramsLine)
	}

	cases := []struct {
		name      string
		toolkit   string
		fragments []string
	}{
		{"unknown tool key", parameterless("    paramz: []\n"), []string{"acme.yaml", `tool "ping"`, "paramz"}},
		{"unknown toolkit key", "provider: acme\nnmae: A\nname: A\ndescription: d\ntools: []\n", []string{"acme.yaml", "nmae"}},
		{"unknown param key", swap("        required: true\n", "        required: true\n        requird: true\n"), []string{`tool "get_widget"`, `param "widgetId"`, "requird"}},
		{"params omitted", parameterless(""), []string{`tool "ping"`, "params"}},
		{"params null", parameterless("    params: null\n"), []string{`tool "ping"`, "params"}},
		{"params a mapping", parameterless("    params: {}\n"), []string{`tool "ping"`, "params"}},
		{"integer id", swap("id: get_widget", "id: 123"), []string{"acme.yaml", "tool #1", "id"}},
		{"required yes", swap("required: true", "required: yes"), []string{`tool "get_widget"`, `param "widgetId"`, "required"}},
		{"required banana", swap("required: true", "required: banana"), []string{"acme.yaml", `tool "get_widget"`, `param "widgetId"`, "required"}},
		{"enum null", swap("required: true\n", "required: true\n        enum: null\n"), []string{`tool "get_widget"`, "enum"}},
		{"transform empty", swap("effect: read", "effect: read\n    transform: \"\""), []string{`tool "get_widget"`, "transform"}},
		{"transform a number", swap("effect: read", "effect: read\n    transform: 7"), []string{`tool "get_widget"`, "transform"}},
		{"name a number", swap(`name: "Get widget"`, "name: 5"), []string{`tool "get_widget"`, "name"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantLoadError(t, toolkitFS(map[string]string{"acme.yaml": tc.toolkit}), tc.fragments...)
		})
	}
}

func TestToolkitAcceptsNullTransformAndPagination(t *testing.T) {
	tool := strings.Replace(validTool, "effect: read", "effect: read\n    transform: null\n    pagination: null\n    response: null", 1)
	c := mustLoad(t, toolkitFS(map[string]string{"acme.yaml": acmeToolkit(tool)}))
	tk, _ := c.Toolkit("acme")
	if got := tk.Tools[0]; got.Transform != "" || got.Pagination != nil || got.Response != nil {
		t.Errorf("tool = %+v, want transform, pagination and response unset", got)
	}
}

func TestIntegerEnumAcceptsIntegralFloats(t *testing.T) {
	tool := `  - id: list_widgets
    name: "List"
    description: "List widgets"
    operation: "GET /widgets"
    effect: read
    params:
      - name: n
        type: integer
        required: false
        enum: [1.0, 2e0, 3]
        description: "Page size"
`
	c := mustLoad(t, toolkitFS(map[string]string{"acme.yaml": acmeToolkit(tool)}))
	tk, _ := c.Toolkit("acme")
	if got := tk.Tools[0].Params[0].Enum; !slices.Equal(got, []any{1, 2, 3}) {
		t.Errorf("enum = %#v, want [1 2 3]", got)
	}

	fractional := strings.Replace(tool, "[1.0, 2e0, 3]", "[1.5]", 1)
	wantLoadError(t, toolkitFS(map[string]string{"acme.yaml": acmeToolkit(fractional)}), `param "n"`, "enum", "1.5")
}

func TestToolkitRejectsUnusableDefinitions(t *testing.T) {
	swap := func(old, new string) string {
		return acmeToolkit(strings.Replace(validTool, old, new, 1))
	}
	optionalPlaceholder := func(extra string) string {
		return swap("        required: true\n", "        required: false\n"+extra)
	}

	cases := []struct {
		name      string
		toolkit   string
		fragments []string
	}{
		{"kebab names collide", acmeToolkit(validTool + strings.Replace(validTool, "id: get_widget", "id: get-widget", 1)),
			[]string{"acme.yaml", "get-widget", "get_widget"}},
		{"duplicate param", swap(`        description: "Widget ID"`+"\n", `        description: "Widget ID"`+"\n"+
			"      - name: widgetId\n        type: string\n        required: false\n        description: \"again\"\n"),
			[]string{`tool "get_widget"`, `param "widgetId"`, "duplicate"}},
		{"unclosed brace", swap("GET /widgets/{widgetId}", "GET /widgets/{widgetId"), []string{`tool "get_widget"`, "unbalanced"}},
		{"stray closing brace", swap("GET /widgets/{widgetId}", "GET /widgets/{widgetId}}"), []string{`tool "get_widget"`, "unbalanced"}},
		{"optional placeholder without default", optionalPlaceholder(""), []string{`tool "get_widget"`, "widgetId", "required"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantLoadError(t, toolkitFS(map[string]string{"acme.yaml": tc.toolkit}), tc.fragments...)
		})
	}

	t.Run("optional placeholder with default loads", func(t *testing.T) {
		mustLoad(t, toolkitFS(map[string]string{"acme.yaml": optionalPlaceholder("        default: w1\n")}))
	})
}
