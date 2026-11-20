package catalog

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

type nodeKind int

const (
	kindString nodeKind = iota
	kindBool
	kindAny
	kindList
	kindObject
	kindStringMap
)

// shape describes the YAML a field must hold; list items are named in errors by noun and labelKey.
type shape struct {
	kind     nodeKind
	required bool
	nullable bool
	nonEmpty bool
	fields   map[string]shape
	item     *shape
	noun     string
	labelKey string
}

var (
	optionalString = shape{kind: kindString}
	requiredString = shape{kind: kindString, required: true}

	paramShape = shape{kind: kindObject, fields: map[string]shape{
		"name":        requiredString,
		"type":        requiredString,
		"required":    {kind: kindBool, required: true},
		"default":     {kind: kindAny, nullable: true},
		"enum":        {kind: kindList, item: &shape{kind: kindAny}, noun: "value"},
		"description": requiredString,
	}}

	toolShape = shape{kind: kindObject, fields: map[string]shape{
		"id":          requiredString,
		"name":        requiredString,
		"description": requiredString,
		"operation":   requiredString,
		"effect":      requiredString,
		"transform":   {kind: kindString, nullable: true, nonEmpty: true},
		"params":      {kind: kindList, required: true, item: &paramShape, noun: "param", labelKey: "name"},
		"pagination": {kind: kindObject, nullable: true, fields: map[string]shape{
			"type":                  requiredString,
			"cursor_param":          requiredString,
			"cursor_response_field": {kind: kindString, nullable: true},
		}},
		"response": {kind: kindObject, nullable: true, fields: map[string]shape{
			"type":   optionalString,
			"fields": {kind: kindStringMap},
		}},
	}}

	toolkitShape = shape{kind: kindObject, fields: map[string]shape{
		"provider":    requiredString,
		"command":     optionalString,
		"name":        requiredString,
		"description": requiredString,
		"tools":       {kind: kindList, required: true, item: &toolShape, noun: "tool", labelKey: "id"},
	}}
)

// check reports the first place n, the value of field under ctx, does not match s.
func (s shape) check(n *yaml.Node, ctx []string, field string) error {
	n = resolveAlias(n)
	fail := func(problem string) error {
		return locate(ctx, fmt.Sprintf("'%s' %s (line %d)", field, problem, n.Line))
	}
	if isNull(n) {
		if s.nullable {
			return nil
		}
		return fail("must not be null")
	}
	switch s.kind {
	case kindAny:
	case kindString:
		if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
			return fail("must be a string")
		}
		if s.nonEmpty && n.Value == "" {
			return fail("must not be empty")
		}
	case kindBool:
		if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!bool" {
			return fail("must be true or false")
		}
	case kindStringMap:
		if n.Kind != yaml.MappingNode {
			return fail("must be a mapping")
		}
		for i := 0; i < len(n.Content); i += 2 {
			if err := optionalString.check(n.Content[i+1], ctx, field+"."+n.Content[i].Value); err != nil {
				return err
			}
		}
	case kindList:
		if n.Kind != yaml.SequenceNode {
			return fail("must be a list")
		}
		for i, item := range n.Content {
			if err := s.item.check(item, append(slices.Clip(ctx), s.itemLabel(item, i)), ""); err != nil {
				return err
			}
		}
	case kindObject:
		if n.Kind != yaml.MappingNode {
			return fail("must be a mapping")
		}
		return s.checkFields(n, ctx)
	}
	return nil
}

func (s shape) checkFields(n *yaml.Node, ctx []string) error {
	var present []string
	for i := 0; i < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		field, known := s.fields[key.Value]
		if !known {
			return locate(ctx, fmt.Sprintf("unknown field '%s' (line %d)", key.Value, key.Line))
		}
		present = append(present, key.Value)
		if err := field.check(value, ctx, key.Value); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(s.fields) {
		if s.fields[name].required && !slices.Contains(present, name) {
			return locate(ctx, fmt.Sprintf("missing required field '%s' (line %d)", name, n.Line))
		}
	}
	return nil
}

func (s shape) itemLabel(item *yaml.Node, i int) string {
	if label := scalarField(item, s.labelKey); label != "" {
		return fmt.Sprintf("%s %q", s.noun, label)
	}
	return fmt.Sprintf("%s #%d", s.noun, i+1)
}

func locate(ctx []string, problem string) error {
	return errors.New(strings.Join(append(slices.Clip(ctx), problem), ": "))
}

func scalarField(n *yaml.Node, key string) string {
	n = resolveAlias(n)
	if n.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if v := resolveAlias(n.Content[i+1]); n.Content[i].Value == key && v.Kind == yaml.ScalarNode && v.ShortTag() == "!!str" {
			return v.Value
		}
	}
	return ""
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
