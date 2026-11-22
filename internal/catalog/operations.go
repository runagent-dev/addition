package catalog

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	operationIDRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)
	operationParam = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)
	hostRE         = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	operationTypes = []string{"string", "integer", "number", "boolean", "object"}
)

// OperationSet is every API operation an app's own API description lists, imported rather than
// curated. It lets an agent discover endpoints and the scope each one needs; `addition call` runs them.
type OperationSet struct {
	Provider   string          `yaml:"provider"`
	Host       string          `yaml:"host"`
	Source     OperationSource `yaml:"source"`
	Operations []Operation     `yaml:"operations"`
}

// OperationSource records which API description an OperationSet was imported from.
type OperationSource struct {
	Kind     string `yaml:"kind"`
	ID       string `yaml:"id"`
	Revision string `yaml:"revision"`
	Docs     string `yaml:"docs,omitempty"`
}

type Operation struct {
	ID      string `yaml:"id"`
	Summary string `yaml:"summary"`
	Method  string `yaml:"method"`
	Path    string `yaml:"path"`
	// Effect is derived from the verb and name, never from a description; policies rely on it.
	Effect Effect           `yaml:"effect"`
	Scopes []string         `yaml:"scopes,omitempty"`
	Params []OperationParam `yaml:"params,omitempty"`
	Body   *OperationBody   `yaml:"body,omitempty"`
	// UploadPath is the separate path for media uploads, when the operation takes one.
	UploadPath string `yaml:"upload_path,omitempty"`
}

type OperationParam struct {
	Name        string   `yaml:"name"`
	In          string   `yaml:"in"`
	Type        string   `yaml:"type"`
	Required    bool     `yaml:"required,omitempty"`
	Default     string   `yaml:"default,omitempty"`
	Enum        []string `yaml:"enum,omitempty"`
	Pattern     string   `yaml:"pattern,omitempty"`
	Description string   `yaml:"description,omitempty"`
}

// OperationBody names the request schema and lists its top-level fields.
type OperationBody struct {
	Schema string      `yaml:"schema"`
	Fields []BodyField `yaml:"fields,omitempty"`
}

type BodyField struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`
	Description string `yaml:"description,omitempty"`
}

// Operations returns the imported operations of a provider, reading and checking its file on first use
// so commands that never list operations do not pay for parsing them.
func (c *Catalog) Operations(providerID string) (*OperationSet, bool, error) {
	if !slices.Contains(c.operationFiles, providerID) {
		return nil, false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if set, ok := c.operations[providerID]; ok {
		return set, true, nil
	}
	set, err := loadOperationSet(c.fsys, providerID, c.providers)
	if err != nil {
		return nil, false, err
	}
	c.operations[providerID] = set
	return set, true, nil
}

// OperationProviders lists the providers that have imported operations, sorted.
func (c *Catalog) OperationProviders() []string {
	return slices.Clone(c.operationFiles)
}

// CheckOperations reads and validates every operations file.
func (c *Catalog) CheckOperations() error {
	for _, id := range c.operationFiles {
		if _, _, err := c.Operations(id); err != nil {
			return err
		}
	}
	return nil
}

func listOperationFiles(fsys fs.FS) ([]string, error) {
	files, err := yamlFiles(fsys, "operations")
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(files))
	for i, f := range files {
		ids[i] = strings.TrimSuffix(strings.TrimSuffix(path.Base(f), ".yaml"), ".yml")
	}
	slices.Sort(ids)
	return ids, nil
}

func loadOperationSet(fsys fs.FS, providerID string, providers map[string]*Provider) (*OperationSet, error) {
	file := path.Join("operations", providerID+".yaml")
	set, err := decodeOperationSet(fsys, file)
	if err != nil {
		return nil, err
	}
	if set.Provider != providerID {
		return nil, fmt.Errorf("%s: provider %q does not match the file name", file, set.Provider)
	}
	provider, ok := providers[set.Provider]
	if !ok {
		return nil, fmt.Errorf("%s: unknown provider %q", file, set.Provider)
	}
	if err := ValidateOperationSet(set, provider); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return set, nil
}

func decodeOperationSet(fsys fs.FS, file string) (*OperationSet, error) {
	doc, err := decodeDocument(fsys, file)
	if err != nil {
		return nil, err
	}
	canonical, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(canonical))
	dec.KnownFields(true)
	var set OperationSet
	if err := dec.Decode(&set); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &set, nil
}

// ValidateOperationSet checks set; with a provider it also checks that every operation stays on the
// provider's allowed host and path prefixes, so no listed operation is one `addition call` would refuse.
func ValidateOperationSet(set *OperationSet, provider *Provider) error {
	if set.Provider == "" {
		return fmt.Errorf("missing required field 'provider'")
	}
	if !hostRE.MatchString(set.Host) {
		return fmt.Errorf("host %q must be a bare lowercase host name", set.Host)
	}
	if provider != nil && !slices.Contains(provider.API.Hosts(), set.Host) {
		return fmt.Errorf("host %q is not in the provider's allowed_hosts", set.Host)
	}
	if set.Source.Kind == "" || set.Source.ID == "" {
		return fmt.Errorf("source needs 'kind' and 'id'")
	}
	if len(set.Operations) == 0 {
		return fmt.Errorf("no operations")
	}
	seen := map[string]bool{}
	for i := range set.Operations {
		op := &set.Operations[i]
		if seen[op.ID] {
			return fmt.Errorf("duplicate operation id %q", op.ID)
		}
		seen[op.ID] = true
		if err := validateOperation(op, provider); err != nil {
			return fmt.Errorf("operation %q: %w", op.ID, err)
		}
	}
	return nil
}

func validateOperation(op *Operation, provider *Provider) error {
	if !operationIDRE.MatchString(op.ID) {
		return fmt.Errorf("id must be letters, digits, dots, dashes and underscores")
	}
	if !operationRE.MatchString(op.Method + " " + op.Path) {
		return fmt.Errorf("method %q with path %q must be GET, POST, PUT, PATCH or DELETE and a path starting with /", op.Method, op.Path)
	}
	if !PlainPath(strings.NewReplacer("{", "x", "}", "x").Replace(op.Path)) {
		return fmt.Errorf("path %q must be a plain path (no dot segments, double slashes, backslashes or spaces)", op.Path)
	}
	if !slices.Contains(effects, op.Effect) {
		return fmt.Errorf("effect %q must be one of read, write, destructive", op.Effect)
	}
	if op.Summary == "" {
		return fmt.Errorf("missing summary")
	}
	for _, s := range op.Scopes {
		if s == "" {
			return fmt.Errorf("empty scope")
		}
	}
	if provider != nil && !pathAllowed(provider.API.AllowedPathPrefixes, op.Path) {
		return fmt.Errorf("path %q is outside the provider's allowed_path_prefixes", op.Path)
	}
	if op.UploadPath != "" && provider != nil && !pathAllowed(provider.API.AllowedPathPrefixes, op.UploadPath) {
		return fmt.Errorf("upload_path %q is outside the provider's allowed_path_prefixes", op.UploadPath)
	}
	return validateOperationParams(op)
}

func pathAllowed(prefixes []string, p string) bool {
	return len(prefixes) == 0 || slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(p, prefix) })
}

func validateOperationParams(op *Operation) error {
	pathParams := map[string]bool{}
	names := map[string]bool{}
	for _, p := range op.Params {
		if !operationParam.MatchString(p.Name) {
			return fmt.Errorf("param %q: name must be letters, digits, dots, dashes and underscores", p.Name)
		}
		if names[p.Name] {
			return fmt.Errorf("param %q: duplicate", p.Name)
		}
		names[p.Name] = true
		if p.In != "path" && p.In != "query" {
			return fmt.Errorf("param %q: in %q must be path or query", p.Name, p.In)
		}
		if !slices.Contains(operationTypes, strings.TrimSuffix(p.Type, "[]")) {
			return fmt.Errorf("param %q: type %q must be one of %s, optionally followed by []", p.Name, p.Type, strings.Join(operationTypes, ", "))
		}
		if p.In == "path" {
			pathParams[p.Name] = true
		}
	}
	if strings.ContainsAny(placeholderRE.ReplaceAllString(op.Path, ""), "{}") {
		return fmt.Errorf("path %q has unbalanced '{' or '}'", op.Path)
	}
	inPath := map[string]bool{}
	for _, m := range placeholderRE.FindAllStringSubmatch(op.Path, -1) {
		inPath[m[1]] = true
		if !pathParams[m[1]] {
			return fmt.Errorf("path placeholder {%s} is not declared as a path param", m[1])
		}
	}
	for name := range pathParams {
		if !inPath[name] {
			return fmt.Errorf("path param %q is not a placeholder in path %q", name, op.Path)
		}
	}
	return nil
}
