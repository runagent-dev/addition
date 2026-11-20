package catalog

import (
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Effect says what a tool does to the account; policies key off it.
type Effect string

const (
	EffectRead        Effect = "read"
	EffectWrite       Effect = "write"
	EffectDestructive Effect = "destructive"
)

var (
	effects       = []Effect{EffectRead, EffectWrite, EffectDestructive}
	paramTypes    = []string{"string", "integer", "boolean", "string[]", "object"}
	operationRE   = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE) (/\S*)$`)
	placeholderRE = regexp.MustCompile(`\{([^{}]*)\}`)
	commandRE     = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

// Toolkit is the curated tool set for one provider.
type Toolkit struct {
	Provider    string `yaml:"provider"`
	Command     string `yaml:"command"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Tools       []Tool `yaml:"tools"`
}

type Tool struct {
	ID          string      `yaml:"id"`
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Operation   string      `yaml:"operation"`
	Effect      Effect      `yaml:"effect"`
	Transform   string      `yaml:"transform"`
	Params      []Param     `yaml:"params"`
	Pagination  *Pagination `yaml:"pagination"`
	Response    *Response   `yaml:"response"`
	// Method and Path are parsed from Operation.
	Method string `yaml:"-"`
	Path   string `yaml:"-"`
}

// CLIName is the tool's kebab-case command name.
func (t Tool) CLIName() string {
	return strings.ReplaceAll(t.ID, "_", "-")
}

type Param struct {
	Name        string
	Type        string
	Required    bool
	Default     any
	Enum        []any
	Description string

	requiredDeclared bool
}

func (p *Param) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		Name        string `yaml:"name"`
		Type        string `yaml:"type"`
		Required    *bool  `yaml:"required"`
		Default     any    `yaml:"default"`
		Enum        []any  `yaml:"enum"`
		Description string `yaml:"description"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	*p = Param{
		Name: raw.Name, Type: raw.Type, Default: raw.Default, Enum: raw.Enum, Description: raw.Description,
		Required: raw.Required != nil && *raw.Required, requiredDeclared: raw.Required != nil,
	}
	return nil
}

type Pagination struct {
	Type                string `yaml:"type"`
	CursorParam         string `yaml:"cursor_param"`
	CursorResponseField string `yaml:"cursor_response_field"`
}

type Response struct {
	Type   string            `yaml:"type"`
	Fields map[string]string `yaml:"fields"`
}

func loadToolkits(fsys fs.FS, providers map[string]*Provider) (map[string]*Toolkit, error) {
	files, err := yamlFiles(fsys, "toolkits")
	if err != nil {
		return nil, err
	}
	toolkits := map[string]*Toolkit{}
	fileOf := map[string]string{}
	commandOwner := map[string]string{}
	for _, file := range files {
		var tk Toolkit
		if err := decodeFile(fsys, file, &tk); err != nil {
			return nil, err
		}
		if err := validateToolkit(&tk, providers); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if prev, dup := fileOf[tk.Provider]; dup {
			return nil, fmt.Errorf("%s: duplicate toolkit for provider %q (also in %s)", file, tk.Provider, prev)
		}
		if owner, taken := commandOwner[tk.Command]; taken {
			return nil, fmt.Errorf("%s: command %q is already used by the %s toolkit", file, tk.Command, owner)
		}
		toolkits[tk.Provider] = &tk
		fileOf[tk.Provider] = file
		commandOwner[tk.Command] = tk.Provider
	}
	return toolkits, nil
}

func validateToolkit(tk *Toolkit, providers map[string]*Provider) error {
	if tk.Provider == "" {
		return fmt.Errorf("missing required field 'provider'")
	}
	if _, ok := providers[tk.Provider]; !ok {
		return fmt.Errorf("provider %q is not defined in providers/", tk.Provider)
	}
	if tk.Name == "" || tk.Description == "" {
		return fmt.Errorf("toolkit %q needs a non-empty 'name' and 'description'", tk.Provider)
	}
	if tk.Command == "" {
		tk.Command = tk.Provider
	}
	if !commandRE.MatchString(tk.Command) {
		return fmt.Errorf("command %q must be lowercase letters, digits and dashes", tk.Command)
	}
	if tk.Tools == nil {
		return fmt.Errorf("'tools' must be a list")
	}

	seen := map[string]bool{}
	for i := range tk.Tools {
		tool := &tk.Tools[i]
		if tool.ID == "" {
			return fmt.Errorf("tool #%d: missing required field 'id'", i+1)
		}
		if seen[tool.ID] {
			return fmt.Errorf("tool %q: duplicate tool id", tool.ID)
		}
		seen[tool.ID] = true
		if err := validateTool(tool); err != nil {
			return fmt.Errorf("tool %q: %w", tool.ID, err)
		}
	}
	return nil
}

func validateTool(tool *Tool) error {
	if tool.Name == "" || tool.Description == "" {
		return fmt.Errorf("needs a non-empty 'name' and 'description'")
	}
	if tool.Operation == "" {
		return fmt.Errorf("missing required field 'operation'")
	}
	m := operationRE.FindStringSubmatch(tool.Operation)
	if m == nil {
		return fmt.Errorf("operation %q must be 'METHOD /path' with METHOD one of GET, POST, PUT, PATCH, DELETE", tool.Operation)
	}
	tool.Method, tool.Path = m[1], m[2]

	if !slices.Contains(effects, tool.Effect) {
		return fmt.Errorf("effect %q must be one of read, write, destructive", tool.Effect)
	}
	if tool.Transform != "" {
		provider, name, ok := strings.Cut(tool.Transform, ":")
		if !ok || provider == "" || name == "" {
			return fmt.Errorf("transform %q must be '{provider}:{transform_name}'", tool.Transform)
		}
	}

	declared := map[string]bool{}
	for _, p := range tool.Params {
		if err := validateParam(p); err != nil {
			return err
		}
		declared[p.Name] = true
	}
	for _, m := range placeholderRE.FindAllStringSubmatch(tool.Path, -1) {
		if !declared[m[1]] {
			return fmt.Errorf("path placeholder {%s} is not declared as a param", m[1])
		}
	}

	if p := tool.Pagination; p != nil {
		if !slices.Contains([]string{"cursor", "offset", "page"}, p.Type) {
			return fmt.Errorf("pagination.type %q must be cursor, offset or page", p.Type)
		}
		if p.CursorParam == "" {
			return fmt.Errorf("pagination.cursor_param is required")
		}
	}
	if r := tool.Response; r != nil && r.Type != "array" {
		r.Type = "object"
	}
	return nil
}

func validateParam(p Param) error {
	if p.Name == "" {
		return fmt.Errorf("param without a 'name'")
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("param %q: %s", p.Name, fmt.Sprintf(format, args...))
	}
	if !slices.Contains(paramTypes, p.Type) {
		return fail("type %q must be one of %s", p.Type, strings.Join(paramTypes, ", "))
	}
	if !p.requiredDeclared {
		return fail("'required' must be set to true or false")
	}
	if p.Description == "" {
		return fail("missing required field 'description'")
	}
	if p.Enum == nil {
		return nil
	}
	var matchesType func(any) bool
	switch p.Type {
	case "string":
		matchesType = func(v any) bool { _, ok := v.(string); return ok }
	case "integer":
		matchesType = func(v any) bool { _, ok := v.(int); return ok }
	default:
		return fail("'enum' is only allowed on string or integer params, not %s", p.Type)
	}
	for _, v := range p.Enum {
		if !matchesType(v) {
			return fail("enum value %#v is not a %s", v, p.Type)
		}
	}
	return nil
}
