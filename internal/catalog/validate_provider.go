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
	root.oneOf("auth_type", "oauth2", "api_key", "basic")
	if fields["auth_type"] == "oauth2" {
		if o, ok := root.mapping("oauth2"); ok {
			validateOAuth2(o)
		}
	}
	if api, ok := root.mapping("api"); ok {
		api.nonEmptyString("base_url")
		api.nonEmptyString("auth_header")
		api.nonEmptyString("auth_prefix")
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
