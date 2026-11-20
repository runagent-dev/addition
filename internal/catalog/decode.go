package catalog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strconv"

	"gopkg.in/yaml.v3"
)

var decimalRE = regexp.MustCompile(`^[-+]?[0-9]+$`)

// decodeDocument reads file as exactly one YAML document with plain string keys and YAML 1.2 core scalars.
func decodeDocument(fsys fs.FS, file string) (*yaml.Node, error) {
	data, err := fs.ReadFile(fsys, file)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: empty file", file)
		}
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%s: must hold one document, found another at line %d", file, extra.Line)
		}
		return nil, fmt.Errorf("%s: after the first document: %w", file, err)
	}
	if err := normalize(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return doc.Content[0], nil
}

// normalize rejects keys that are not plain strings and re-resolves plain scalars by YAML 1.2 core rules.
func normalize(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		resolveCoreScalar(n)
	}
	for _, child := range n.Content {
		if err := normalize(child); err != nil {
			return err
		}
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			if err := checkKey(n.Content[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkKey(key *yaml.Node) error {
	switch {
	case key.Kind == yaml.AliasNode:
		return fmt.Errorf("line %d: mapping key must be a plain string, not an alias", key.Line)
	case key.Anchor != "":
		return fmt.Errorf("line %d: mapping key must be a plain string, not anchored", key.Line)
	case key.Kind != yaml.ScalarNode || key.ShortTag() != "!!str":
		return fmt.Errorf("line %d: mapping key must be a string", key.Line)
	}
	return nil
}

// resolveCoreScalar reads plain dates as strings and plain digit runs as base-10 integers, so 010 is 10.
func resolveCoreScalar(n *yaml.Node) {
	if n.Style != 0 {
		return
	}
	if n.ShortTag() == "!!timestamp" {
		n.Tag = "!!str"
		return
	}
	if !decimalRE.MatchString(n.Value) {
		return
	}
	if v, err := strconv.ParseInt(n.Value, 10, 64); err == nil {
		n.Tag, n.Value = "!!int", strconv.FormatInt(v, 10)
	}
}
