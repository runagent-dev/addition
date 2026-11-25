package catalog

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// fieldChecker validates one YAML mapping and keeps the first failure.
type fieldChecker struct {
	fields map[string]any
	prefix string
	err    *error
}

func (c fieldChecker) fail(key, format string, args ...any) {
	if *c.err == nil {
		*c.err = fmt.Errorf("field '%s%s' %s", c.prefix, key, fmt.Sprintf(format, args...))
	}
}

func (c fieldChecker) nonEmptyString(key string) {
	if s, ok := c.fields[key].(string); !ok || s == "" {
		c.fail(key, "must be a non-empty string")
	}
}

// anyString accepts any string, the empty one included.
func (c fieldChecker) anyString(key string) {
	if _, ok := c.fields[key].(string); !ok {
		c.fail(key, "must be a string")
	}
}

func (c fieldChecker) optionalString(key string) {
	if v, present := c.fields[key]; present && v != nil {
		c.nonEmptyString(key)
	}
}

func (c fieldChecker) boolean(key string) {
	if _, ok := c.fields[key].(bool); !ok {
		c.fail(key, "must be a boolean, got %#v", c.fields[key])
	}
}

func (c fieldChecker) oneOf(key string, allowed ...string) {
	if s, ok := c.fields[key].(string); !ok || !slices.Contains(allowed, s) {
		c.fail(key, "must be one of %s, got %#v", strings.Join(allowed, ", "), c.fields[key])
	}
}

func (c fieldChecker) stringList(key string) {
	list, ok := c.fields[key].([]any)
	if !ok || slices.ContainsFunc(list, func(v any) bool { _, isString := v.(string); return !isString }) {
		c.fail(key, "must be a list of strings")
	}
}

// onlyKeys rejects any field not in allowed, so a typo cannot silently drop config.
func (c fieldChecker) onlyKeys(allowed ...string) {
	for _, key := range sortedKeys(c.fields) {
		if !slices.Contains(allowed, key) {
			c.fail(key, "is not a known field")
		}
	}
}

// mapping checks key holds a mapping and returns a checker for its fields.
func (c fieldChecker) mapping(key string) (fieldChecker, bool) {
	m, ok := c.fields[key].(map[string]any)
	if !ok {
		c.fail(key, "must be a mapping")
	}
	return fieldChecker{fields: m, prefix: c.prefix + key + ".", err: c.err}, ok
}

// validateProvider checks a merged provider config so YAML typos fail at load, not deep in OAuth.
func validateProvider(fields map[string]any, opts Options) error {
	var err error
	root := fieldChecker{fields: fields, err: &err}

	root.nonEmptyString("name")
	root.oneOf("auth_type", MethodOAuth2, MethodAPIKey, "basic")
	if fields["oauth2"] != nil || fields["auth_type"] == MethodOAuth2 {
		if o, ok := root.mapping("oauth2"); ok {
			validateOAuth2(o)
		}
	}
	apiFields, _ := fields["api"].(map[string]any)
	if fields["api_key"] != nil || fields["auth_type"] == MethodAPIKey {
		if k, ok := root.mapping("api_key"); ok {
			validateAPIKey(k, apiFields)
		}
	}
	if api, ok := root.mapping("api"); ok {
		api.nonEmptyString("base_url")
		api.nonEmptyString("auth_header")
		api.anyString("auth_prefix")
		validateAllowedHosts(api, opts)
		validatePathPrefixes(api)
		validateProbe(api)
	}
	validateDoctor(root)
	return err
}

// hostEntryPattern is a lowercase host name with an optional port: no scheme, userinfo or path.
var hostEntryPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)

// validateAllowedHosts checks the optional api.allowed_hosts and that base_url points at a listed host.
func validateAllowedHosts(api fieldChecker, opts Options) {
	raw, present := api.fields["allowed_hosts"]
	if !present || raw == nil {
		checkBaseURLPort(api, opts)
		return
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		api.fail("allowed_hosts", "must be a non-empty list of host names")
		return
	}
	var hosts []string
	for _, entry := range list {
		host, isString := entry.(string)
		if !isString || !hostEntryPattern.MatchString(host) {
			api.fail("allowed_hosts", "entries must be lowercase host names, with an optional port, got %#v", entry)
			return
		}
		if strings.Contains(host, ":") && !opts.AllowHostPorts {
			api.fail("allowed_hosts", "entry %q names a port; access tokens are sent to port 443 only", host)
			return
		}
		hosts = append(hosts, host)
	}
	checkBaseURLPort(api, opts)
	base, err := url.Parse(fmt.Sprint(api.fields["base_url"]))
	if err != nil || !slices.Contains(hosts, HostKey(base)) {
		api.fail("base_url", "has a host that is not listed in api.allowed_hosts")
	}
}

// validatePathPrefixes checks the optional api.allowed_path_prefixes: plain paths that start with /.
func validatePathPrefixes(api fieldChecker) {
	raw, present := api.fields["allowed_path_prefixes"]
	if !present || raw == nil {
		return
	}
	list, ok := raw.([]any)
	if !ok {
		api.fail("allowed_path_prefixes", "must be a list of paths")
		return
	}
	for _, entry := range list {
		prefix, isString := entry.(string)
		if !isString || !PlainPath(prefix) || strings.ContainsAny(prefix, "?#%") {
			api.fail("allowed_path_prefixes", "entries must start with / and hold no dot segments, empty segments, escapes, query or whitespace, got %#v", entry)
			return
		}
	}
}

func validateOAuth2(o fieldChecker) {
	o.nonEmptyString("authorize_url")
	o.nonEmptyString("token_url")
	o.optionalString("revoke_url")
	if _, present := o.fields["revoke_scope"]; present {
		o.oneOf("revoke_scope", revokeScopeToken, revokeScopeGrant)
	}
	o.boolean("refresh_supported")
	o.boolean("pkce_supported")
	o.boolean("pkce_required")
	if o.fields["pkce_required"] == true && o.fields["pkce_supported"] == false {
		o.fail("pkce_required", "is true but 'pkce_supported' is false")
	}
	o.stringList("default_scopes")
	o.oneOf("scopes_delimiter", "space", "comma")
	o.mapping("authorize_params")
	o.mapping("token_params")
	o.oneOf("token_response_format", "json", "form")
	o.oneOf("client_auth_method", "body", "header")
	if o.fields["token_response_mapping"] != nil {
		if m, ok := o.mapping("token_response_mapping"); ok {
			m.nonEmptyString("access_token")
			m.nonEmptyString("refresh_token")
			m.nonEmptyString("expires_in")
		}
	}
	if o.fields["identity"] != nil {
		if id, ok := o.mapping("identity"); ok {
			id.nonEmptyString("url")
			id.nonEmptyString("field")
			id.nonEmptyString("subject_field")
			id.onlyKeys("url", "field", "subject_field")
		}
	}
}

// checkBaseURLPort refuses a base_url with an explicit port: tokens are sent to port 443 only.
func checkBaseURLPort(api fieldChecker, opts Options) {
	if base, err := url.Parse(fmt.Sprint(api.fields["base_url"])); err == nil && base.Port() != "" && !opts.AllowHostPorts {
		api.fail("base_url", "names a port; access tokens are sent to port 443 only")
	}
}

var servicePattern = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+$`)

// validateProbe checks the optional api.service and api.probe_path that `addition doctor` uses.
func validateProbe(api fieldChecker) {
	api.optionalString("service")
	if s, ok := api.fields["service"].(string); ok && !servicePattern.MatchString(s) {
		api.fail("service", "must be a lowercase API host name such as gmail.googleapis.com, got %q", s)
	}
	api.optionalString("probe_path")
	if s, ok := api.fields["probe_path"].(string); ok && (!strings.HasPrefix(s, "/") || strings.ContainsFunc(s, isSpaceOrControl)) {
		api.fail("probe_path", "must be an absolute path without whitespace, got %q", s)
	}
}

func isSpaceOrControl(r rune) bool { return r <= ' ' || r == 0x7f }

// validateDoctor checks the optional doctor mapping.
func validateDoctor(root fieldChecker) {
	if root.fields["doctor"] == nil {
		return
	}
	doc, ok := root.mapping("doctor")
	if !ok {
		return
	}
	doc.onlyKeys("client_hint", "token_age_warn_days", "token_age_hint")
	doc.optionalString("client_hint")
	doc.optionalString("token_age_hint")
	if days, present := doc.fields["token_age_warn_days"]; present {
		if n, isInt := days.(int); !isInt || n < 1 {
			doc.fail("token_age_warn_days", "must be a positive whole number of days, got %#v", days)
		}
	}
}

var envVarPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// validateAPIKey checks the api_key block. api is the merged api mapping, which the identity path and the probe must fit.
func validateAPIKey(k fieldChecker, api map[string]any) {
	k.onlyKeys("create_url", "instructions", "token_prefix", "env_var", "auth_prefix", "identity")
	validateCreateURL(k)
	if v, present := k.fields["instructions"]; present && v != nil {
		k.stringList("instructions")
	}
	k.optionalString("token_prefix")
	if s, ok := k.fields["token_prefix"].(string); ok && strings.ContainsFunc(s, isSpaceOrControl) {
		k.fail("token_prefix", "must not hold whitespace or control characters")
	}
	k.optionalString("env_var")
	if s, ok := k.fields["env_var"].(string); ok && !envVarPattern.MatchString(s) {
		k.fail("env_var", "must be an upper-case environment variable name such as EXAMPLE_TOKEN, got %q", s)
	}
	if v, present := k.fields["auth_prefix"]; present && v != nil {
		k.anyString("auth_prefix")
		if s, ok := v.(string); ok && strings.ContainsFunc(s, func(r rune) bool { return r < ' ' || r == 0x7f }) {
			k.fail("auth_prefix", "must not hold control characters")
		}
	}
	switch {
	case k.fields["identity"] != nil:
		if id, ok := k.mapping("identity"); ok {
			validateAPIKeyIdentity(id, api)
		}
	case api["probe_path"] == nil:
		k.fail("identity", "is missing and api.probe_path is not set: a token is verified with one of them")
	}
}

// validateCreateURL checks that api_key.create_url is an https page to send the user to, with no credentials in it.
func validateCreateURL(k fieldChecker) {
	k.nonEmptyString("create_url")
	raw, ok := k.fields["create_url"].(string)
	if !ok || raw == "" {
		return
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsFunc(raw, isSpaceOrControl) {
		k.fail("create_url", "must be an https URL without credentials, got %q", raw)
	}
}

func validateAPIKeyIdentity(id fieldChecker, api map[string]any) {
	id.onlyKeys("path", "field", "subject_field")
	id.nonEmptyString("path")
	id.nonEmptyString("field")
	id.nonEmptyString("subject_field")
	path, ok := id.fields["path"].(string)
	if !ok || path == "" {
		return
	}
	if !PlainPath(path) || strings.ContainsAny(path, "?#%") {
		id.fail("path", "must be a plain absolute path on the API's host, without query, escapes or dot segments, got %q", path)
		return
	}
	prefixes, _ := api["allowed_path_prefixes"].([]any)
	if len(prefixes) > 0 && !slices.ContainsFunc(prefixes, func(p any) bool { s, _ := p.(string); return strings.HasPrefix(path, s) }) {
		id.fail("path", "does not start with one of api.allowed_path_prefixes, so the token could not be sent there")
	}
}
