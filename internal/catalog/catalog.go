// Package catalog loads provider and toolkit definitions from YAML.
package catalog

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Catalog is the validated set of providers and the toolkits built on them.
type Catalog struct {
	providers map[string]*Provider
	toolkits  map[string]*Toolkit
}

// Load reads providers/ and toolkits/ from fsys. Files starting with "_" are templates and are skipped.
func Load(fsys fs.FS) (*Catalog, error) {
	providers, err := loadProviders(fsys)
	if err != nil {
		return nil, err
	}
	toolkits, err := loadToolkits(fsys, providers)
	if err != nil {
		return nil, err
	}
	return &Catalog{providers: providers, toolkits: toolkits}, nil
}

// Providers returns every provider sorted by id.
func (c *Catalog) Providers() []*Provider {
	return sortedValues(c.providers)
}

// Provider looks up a provider by id.
func (c *Catalog) Provider(id string) (*Provider, bool) {
	p, ok := c.providers[id]
	return p, ok
}

// IdentityRoot returns the top of id's extends chain, which owns its accounts and OAuth client.
func (c *Catalog) IdentityRoot(id string) (string, error) {
	p, ok := c.providers[id]
	if !ok {
		return "", fmt.Errorf("unknown provider %q", id)
	}
	for p.Extends != "" {
		p = c.providers[p.Extends]
	}
	return p.ID, nil
}

// Toolkits returns every toolkit sorted by provider id.
func (c *Catalog) Toolkits() []*Toolkit {
	return sortedValues(c.toolkits)
}

// Toolkit looks up the toolkit for a provider id.
func (c *Catalog) Toolkit(providerID string) (*Toolkit, bool) {
	tk, ok := c.toolkits[providerID]
	return tk, ok
}

func sortedValues[V any](m map[string]*V) []*V {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]*V, len(keys))
	for i, k := range keys {
		out[i] = m[k]
	}
	return out
}

// yamlFiles lists dir's YAML files in name order; a missing dir has none.
func yamlFiles(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		isYAML := strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
		if e.IsDir() || !isYAML || strings.HasPrefix(name, "_") {
			continue
		}
		files = append(files, path.Join(dir, name))
	}
	return files, nil
}

func decodeFile(fsys fs.FS, file string, out any) error {
	data, err := fs.ReadFile(fsys, file)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}
