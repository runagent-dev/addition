package catalog

import (
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Provider is a provider config with its extends chain already merged in.
type Provider struct {
	ID       string   `yaml:"id"`
	Name     string   `yaml:"name"`
	AuthType string   `yaml:"auth_type"`
	OAuth2   *OAuth2  `yaml:"oauth2"`
	API      API      `yaml:"api"`
	Quirks   []string `yaml:"quirks"`
	// Extends is the direct parent's id, empty for a root provider.
	Extends string `yaml:"-"`
}

type OAuth2 struct {
	AuthorizeURL         string                `yaml:"authorize_url"`
	TokenURL             string                `yaml:"token_url"`
	RevokeURL            string                `yaml:"revoke_url"`
	RefreshSupported     bool                  `yaml:"refresh_supported"`
	PKCESupported        bool                  `yaml:"pkce_supported"`
	PKCERequired         bool                  `yaml:"pkce_required"`
	DefaultScopes        []string              `yaml:"default_scopes"`
	ScopesDelimiter      string                `yaml:"scopes_delimiter"`
	AuthorizeParams      map[string]string     `yaml:"authorize_params"`
	TokenParams          map[string]string     `yaml:"token_params"`
	TokenResponseFormat  string                `yaml:"token_response_format"`
	ClientAuthMethod     string                `yaml:"client_auth_method"`
	TokenResponseMapping *TokenResponseMapping `yaml:"token_response_mapping"`
	Identity             *Identity             `yaml:"identity"`
}

// Identity names the endpoint that tells who an access token belongs to. SubjectField is the
// stable account id that aliases match on; Field is a display name, such as an email.
type Identity struct {
	URL          string `yaml:"url"`
	Field        string `yaml:"field"`
	SubjectField string `yaml:"subject_field"`
}

// TokenResponseMapping holds dot-paths into a non-standard token response.
type TokenResponseMapping struct {
	AccessToken  string `yaml:"access_token"`
	RefreshToken string `yaml:"refresh_token"`
	ExpiresIn    string `yaml:"expires_in"`
}

type API struct {
	BaseURL    string     `yaml:"base_url"`
	AuthHeader string     `yaml:"auth_header"`
	AuthPrefix string     `yaml:"auth_prefix"`
	RateLimit  *RateLimit `yaml:"rate_limit"`
}

type RateLimit struct {
	RequestsPerSecond float64 `yaml:"requests_per_second"`
	RetryAfterHeader  string  `yaml:"retry_after_header"`
}

type rawProvider struct {
	file    string
	extends string
	fields  map[string]any
}

func loadProviders(fsys fs.FS) (map[string]*Provider, error) {
	raws, err := readRawProviders(fsys)
	if err != nil {
		return nil, err
	}
	r := &providerResolver{raws: raws, merged: map[string]map[string]any{}, resolved: map[string]*Provider{}}
	for _, id := range slices.Sorted(maps.Keys(raws)) {
		if err := r.resolve(id, nil); err != nil {
			return nil, err
		}
	}
	return r.resolved, nil
}

func readRawProviders(fsys fs.FS) (map[string]*rawProvider, error) {
	files, err := yamlFiles(fsys, "providers")
	if err != nil {
		return nil, err
	}
	raws := map[string]*rawProvider{}
	for _, file := range files {
		doc, err := decodeDocument(fsys, file)
		if err != nil {
			return nil, err
		}
		var fields map[string]any
		if err := doc.Decode(&fields); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		id, _ := fields["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("%s: missing a non-empty 'id'", file)
		}
		if prev, dup := raws[id]; dup {
			return nil, fmt.Errorf("%s: duplicate provider id %q (also in %s)", file, id, prev.file)
		}
		raw := &rawProvider{file: file, fields: fields}
		if v := fields["extends"]; v != nil {
			parent, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%s: provider %q: 'extends' must be a provider id", file, id)
			}
			raw.extends = parent
		}
		raws[id] = raw
	}
	return raws, nil
}

type providerResolver struct {
	raws     map[string]*rawProvider
	merged   map[string]map[string]any
	resolved map[string]*Provider
}

// resolve merges id onto its resolved parent; chain is the ids currently being resolved.
func (r *providerResolver) resolve(id string, chain []string) error {
	if _, done := r.merged[id]; done {
		return nil
	}
	if slices.Contains(chain, id) {
		return fmt.Errorf("%s: extends cycle: %s", r.raws[id].file, strings.Join(append(chain, id), " -> "))
	}
	raw := r.raws[id]
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: provider %q: %s", raw.file, id, fmt.Sprintf(format, args...))
	}

	fields := maps.Clone(raw.fields)
	if raw.extends != "" {
		if _, ok := r.raws[raw.extends]; !ok {
			return fail("extends unknown provider %q", raw.extends)
		}
		if name, _ := raw.fields["name"].(string); name == "" {
			return fail("uses 'extends: %s' but does not declare its own 'name'", raw.extends)
		}
		if err := r.resolve(raw.extends, append(chain, id)); err != nil {
			return err
		}
		fields = deepMerge(r.merged[raw.extends], raw.fields)
		fields["id"] = id
		fields["name"] = raw.fields["name"]
	}
	delete(fields, "extends")

	if err := validateProvider(fields); err != nil {
		return fail("%v", err)
	}
	p, err := decodeProvider(fields)
	if err != nil {
		return fail("%v", err)
	}
	p.Extends = raw.extends
	r.merged[id] = fields
	r.resolved[id] = p
	return nil
}

// deepMerge overlays child on base: maps merge recursively, everything else (arrays, scalars, null) replaces.
func deepMerge(base, child map[string]any) map[string]any {
	out := maps.Clone(base)
	for key, childVal := range child {
		if key == "extends" {
			continue
		}
		baseMap, baseIsMap := base[key].(map[string]any)
		childMap, childIsMap := childVal.(map[string]any)
		if baseIsMap && childIsMap {
			out[key] = deepMerge(baseMap, childMap)
		} else {
			out[key] = childVal
		}
	}
	return out
}

func decodeProvider(fields map[string]any) (*Provider, error) {
	data, err := yaml.Marshal(fields)
	if err != nil {
		return nil, err
	}
	var p Provider
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}
