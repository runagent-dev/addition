package catalog

import (
	"testing"
	"testing/fstest"
)

const opsProvider = `id: "acme"
name: "Acme"
auth_type: "oauth2"
` + oauth2Block + `api:
  base_url: "https://example.com"
  allowed_path_prefixes: ["/v2/"]
  auth_header: "Authorization"
  auth_prefix: "Bearer"
`

const validOperations = `provider: acme
host: example.com
source:
  kind: google-discovery
  id: "acme:v2"
  revision: "20260101"
operations:
  - id: widgets.get
    summary: "Gets one widget."
    method: GET
    path: /v2/widgets/{widgetId}
    effect: read
    scopes: ["acme.read"]
    params:
      - name: widgetId
        in: path
        type: string
        required: true
        description: "The widget id."
      - name: view
        in: query
        type: string
        enum: ["basic", "full"]
`

func operationsFS(files map[string]string) fstest.MapFS {
	fsys := providerFS(map[string]string{"acme.yaml": opsProvider})
	for name, body := range files {
		fsys["operations/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func TestOperationsLoad(t *testing.T) {
	c := mustLoad(t, operationsFS(map[string]string{"acme.yaml": validOperations, "_template.yaml": "provider: \"\"\n"}))

	set, ok := c.Operations("acme")
	if !ok {
		t.Fatal("acme operations not loaded")
	}
	op := set.Operations[0]
	if op.ID != "widgets.get" || op.Method != "GET" || op.Path != "/v2/widgets/{widgetId}" || op.Effect != EffectRead {
		t.Errorf("operation = %+v", op)
	}
	if set.Source.Revision != "20260101" || len(op.Params) != 2 || op.Params[1].Enum[1] != "full" {
		t.Errorf("source/params = %+v / %+v", set.Source, op.Params)
	}
	if _, ok := c.Operations("unknown"); ok {
		t.Error("a provider without operations should report none")
	}
	if got := len(c.OperationSets()); got != 1 {
		t.Errorf("OperationSets = %d, want 1", got)
	}
}

func TestOperationsAreOptional(t *testing.T) {
	c := mustLoad(t, operationsFS(nil))
	if got := len(c.OperationSets()); got != 0 {
		t.Errorf("OperationSets = %d, want 0", got)
	}
}

func replaceOnce(t *testing.T, s, old, repl string) string {
	t.Helper()
	i := indexOf(s, old)
	if i < 0 {
		t.Fatalf("fixture has no %q", old)
	}
	return s[:i] + repl + s[i+len(old):]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestOperationsRejectBadData(t *testing.T) {
	dup := validOperations + "  - id: widgets.get\n    summary: x\n    method: GET\n    path: /v2/w\n    effect: read\n"
	for name, tc := range map[string]struct {
		body string
		want []string
	}{
		"unknown provider": {replaceOnce(t, validOperations, "provider: acme", "provider: nope"), []string{"operations/acme.yaml", "unknown provider"}},
		"host not allowed": {replaceOnce(t, validOperations, "host: example.com", "host: evil.example.net"), []string{"host", "allowed_hosts"}},
		"path off prefix":  {replaceOnce(t, validOperations, "/v2/widgets/{widgetId}", "/other/{widgetId}"), []string{"widgets.get", "allowed_path_prefixes"}},
		"duplicate id":     {dup, []string{"duplicate", "widgets.get"}},
		"bad method":       {replaceOnce(t, validOperations, "method: GET", "method: TRACE"), []string{"widgets.get", "method"}},
		"bad effect":       {replaceOnce(t, validOperations, "effect: read", "effect: maybe"), []string{"widgets.get", "effect"}},
		"undeclared path placeholder": {
			replaceOnce(t, validOperations, "/v2/widgets/{widgetId}", "/v2/widgets/{widgetId}/{other}"), []string{"widgets.get", "other"},
		},
		"declared path param not in path": {
			replaceOnce(t, validOperations, "/v2/widgets/{widgetId}", "/v2/widgets"), []string{"widgets.get", "widgetId"},
		},
		"bad param type": {replaceOnce(t, validOperations, "type: string\n        required: true", "type: blob\n        required: true"), []string{"widgetId", "type"}},
		"bad param place": {replaceOnce(t, validOperations, "in: query", "in: cookie"), []string{"view", "in"}},
		"unknown field":   {validOperations + "    colour: red\n", []string{"colour"}},
		"path traversal":  {replaceOnce(t, validOperations, "/v2/widgets/{widgetId}", "/v2/../admin/{widgetId}"), []string{"widgets.get", "path"}},
	} {
		t.Run(name, func(t *testing.T) {
			wantLoadError(t, operationsFS(map[string]string{"acme.yaml": tc.body}), tc.want...)
		})
	}
}

func TestValidateOperationSetWithoutProviders(t *testing.T) {
	c := mustLoad(t, operationsFS(map[string]string{"acme.yaml": validOperations}))
	set, _ := c.Operations("acme")
	if err := ValidateOperationSet(set, nil); err != nil {
		t.Errorf("a valid set must pass without a provider: %v", err)
	}
}
