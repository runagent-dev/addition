// Package specimport turns a vendor's own API description into the operation lists the catalog ships.
package specimport

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/runagent-dev/addition/internal/catalog"
)

type discoveryDoc struct {
	DiscoveryVersion string                       `json:"discoveryVersion"`
	Name             string                       `json:"name"`
	Version          string                       `json:"version"`
	Revision         string                       `json:"revision"`
	RootURL          string                       `json:"rootUrl"`
	ServicePath      string                       `json:"servicePath"`
	DocumentationURL string                       `json:"documentationLink"`
	Parameters       map[string]discoveryParam    `json:"parameters"`
	Schemas          map[string]discoverySchema   `json:"schemas"`
	Resources        map[string]discoveryResource `json:"resources"`
}

type discoveryResource struct {
	Methods   map[string]discoveryMethod   `json:"methods"`
	Resources map[string]discoveryResource `json:"resources"`
}

type discoveryMethod struct {
	ID             string                    `json:"id"`
	HTTPMethod     string                    `json:"httpMethod"`
	Path           string                    `json:"path"`
	Description    string                    `json:"description"`
	ParameterOrder []string                  `json:"parameterOrder"`
	Parameters     map[string]discoveryParam `json:"parameters"`
	Request        *discoveryRef             `json:"request"`
	Scopes         []string                  `json:"scopes"`
	MediaUpload    *struct {
		Protocols struct {
			Simple *struct {
				Path string `json:"path"`
			} `json:"simple"`
		} `json:"protocols"`
	} `json:"mediaUpload"`
}

type discoveryRef struct {
	Ref string `json:"$ref"`
}

type discoveryParam struct {
	Type        string   `json:"type"`
	Location    string   `json:"location"`
	Required    bool     `json:"required"`
	Repeated    bool     `json:"repeated"`
	Deprecated  bool     `json:"deprecated"`
	Default     string   `json:"default"`
	Enum        []string `json:"enum"`
	Pattern     string   `json:"pattern"`
	Description string   `json:"description"`
}

type discoverySchema struct {
	Properties map[string]discoveryProperty `json:"properties"`
}

type discoveryProperty struct {
	Type        string             `json:"type"`
	Ref         string             `json:"$ref"`
	Description string             `json:"description"`
	Items       *discoveryProperty `json:"items"`
}

// GoogleDiscovery reads a Google API Discovery document (discoveryVersion v1) into the operation set for provider.
func GoogleDiscovery(provider string, data []byte) (*catalog.OperationSet, error) {
	var doc discoveryDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("not a Discovery document: %w", err)
	}
	if doc.DiscoveryVersion != "v1" {
		return nil, fmt.Errorf("discoveryVersion %q is not supported, want v1", doc.DiscoveryVersion)
	}
	root, err := url.Parse(doc.RootURL)
	if err != nil || root.Scheme != "https" || root.Host == "" || strings.Trim(root.Path, "/") != "" || root.RawQuery != "" {
		return nil, fmt.Errorf("rootUrl %q must be a plain https origin", doc.RootURL)
	}
	set := &catalog.OperationSet{
		Provider: provider,
		Host:     catalog.HostKey(root),
		Source: catalog.OperationSource{
			Kind: "google-discovery", ID: doc.Name + ":" + doc.Version, Revision: doc.Revision, Docs: doc.DocumentationURL,
		},
	}
	b := builder{doc: &doc}
	if err := b.collect(doc.Resources, &set.Operations); err != nil {
		return nil, err
	}
	slices.SortFunc(set.Operations, func(a, c catalog.Operation) int { return strings.Compare(a.ID, c.ID) })
	return set, nil
}

type builder struct {
	doc *discoveryDoc
}

func (b builder) collect(resources map[string]discoveryResource, out *[]catalog.Operation) error {
	for _, r := range resources {
		for name, m := range r.Methods {
			op, err := b.operation(name, m)
			if err != nil {
				return err
			}
			*out = append(*out, op)
		}
		if err := b.collect(r.Resources, out); err != nil {
			return err
		}
	}
	return nil
}

func (b builder) operation(name string, m discoveryMethod) (catalog.Operation, error) {
	if m.ID == "" {
		return catalog.Operation{}, fmt.Errorf("method %q has no id", name)
	}
	switch m.HTTPMethod {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	default:
		return catalog.Operation{}, fmt.Errorf("%s: unsupported HTTP method %q", m.ID, m.HTTPMethod)
	}
	op := catalog.Operation{
		ID:      m.ID,
		Summary: firstSentence(m.Description, summaryLimit),
		Method:  m.HTTPMethod,
		Path:    flattenPath("/" + strings.TrimPrefix(b.doc.ServicePath+m.Path, "/")),
		Effect:  effectOf(m.HTTPMethod, name),
		Scopes:  sortedUnique(m.Scopes),
		Params:  b.params(m),
		Body:    b.body(m),
	}
	if op.Summary == "" {
		op.Summary = m.ID
	}
	if up := m.MediaUpload; up != nil && up.Protocols.Simple != nil && up.Protocols.Simple.Path != "" {
		op.UploadPath = "/" + strings.TrimPrefix(up.Protocols.Simple.Path, "/")
	}
	return op, nil
}

// flattenPath rewrites reserved-expansion placeholders such as {+name} to {name}.
func flattenPath(p string) string {
	return strings.ReplaceAll(p, "{+", "{")
}

var readVerbs = []string{"list", "get", "batchGet", "search", "query", "count", "lookup", "check", "testIamPermissions"}

// effectOf classifies by verb and method name. An unknown POST is a write: the cost of calling a
// read a write is a confirmation prompt, the cost of the opposite is a silent change.
func effectOf(httpMethod, name string) catalog.Effect {
	switch {
	case httpMethod == "GET":
		return catalog.EffectRead
	case httpMethod == "DELETE", name == "batchDelete", name == "purge", name == "emptyTrash":
		return catalog.EffectDestructive
	case httpMethod == "POST" && slices.Contains(readVerbs, name):
		return catalog.EffectRead
	}
	return catalog.EffectWrite
}

func (b builder) params(m discoveryMethod) []catalog.OperationParam {
	merged := map[string]discoveryParam{}
	if f, ok := b.doc.Parameters["fields"]; ok {
		merged["fields"] = f
	}
	for name, p := range m.Parameters {
		merged[name] = p
	}
	var path, query []catalog.OperationParam
	for name, p := range merged {
		if p.Deprecated || (p.Location != "path" && p.Location != "query") {
			continue
		}
		param := catalog.OperationParam{
			Name: name, In: p.Location, Type: typeOf(p.Type, p.Repeated), Required: p.Required || p.Location == "path",
			Default: p.Default, Enum: p.Enum, Pattern: p.Pattern, Description: firstSentence(p.Description, paramLimit),
		}
		if p.Location == "path" {
			path = append(path, param)
		} else {
			query = append(query, param)
		}
	}
	order := func(list []catalog.OperationParam, rank func(string) int) {
		slices.SortFunc(list, func(a, c catalog.OperationParam) int {
			if d := rank(a.Name) - rank(c.Name); d != 0 {
				return d
			}
			return strings.Compare(a.Name, c.Name)
		})
	}
	order(path, func(n string) int {
		if i := slices.Index(m.ParameterOrder, n); i >= 0 {
			return i
		}
		return len(m.ParameterOrder)
	})
	order(query, func(string) int { return 0 })
	return append(path, query...)
}

func typeOf(t string, repeated bool) string {
	switch t {
	case "string", "integer", "number", "boolean":
	default:
		t = "object"
	}
	if repeated {
		return t + "[]"
	}
	return t
}

func (b builder) body(m discoveryMethod) *catalog.OperationBody {
	if m.Request == nil || m.Request.Ref == "" {
		return nil
	}
	body := &catalog.OperationBody{Schema: m.Request.Ref}
	schema := b.doc.Schemas[m.Request.Ref]
	for name, p := range schema.Properties {
		body.Fields = append(body.Fields, catalog.BodyField{Name: name, Type: propertyType(p), Description: firstSentence(p.Description, paramLimit)})
	}
	slices.SortFunc(body.Fields, func(a, c catalog.BodyField) int { return strings.Compare(a.Name, c.Name) })
	return body
}

func propertyType(p discoveryProperty) string {
	if p.Type == "array" && p.Items != nil {
		return strings.TrimSuffix(propertyType(*p.Items), "[]") + "[]"
	}
	if p.Ref != "" {
		return "object"
	}
	return typeOf(p.Type, false)
}

func sortedUnique(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}
