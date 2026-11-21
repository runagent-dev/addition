package proxy

import (
	"net/url"
	"slices"
	"strings"

	"github.com/runagent-dev/addition/internal/catalog"
)

// ResolveURL turns the caller's path into the URL to request and refuses any target whose host
// is not one of the provider's allowed hosts, so the access token can only go there.
// An absolute path such as /drive/v3/files is resolved against the host of api.base_url;
// an https URL must name an allowed host and carry no userinfo. queryFlags are k=v pairs
// appended after the path's own query.
func ResolveURL(api catalog.API, rawPath string, queryFlags []string) (*url.URL, error) {
	if rawPath == "" || strings.ContainsFunc(rawPath, isControlOrSpace) {
		return nil, badPath()
	}
	target, err := resolveTarget(api, rawPath)
	if err != nil {
		return nil, err
	}
	if _, err := url.ParseQuery(target.RawQuery); err != nil {
		return nil, badPath()
	}
	extra, err := encodeQueryFlags(queryFlags)
	if err != nil {
		return nil, err
	}
	target.RawQuery = joinQuery(target.RawQuery, extra)
	return target, nil
}

func resolveTarget(api catalog.API, rawPath string) (*url.URL, error) {
	switch {
	case strings.HasPrefix(rawPath, "//"):
		return nil, hostNotAllowed(api)
	case strings.HasPrefix(rawPath, "/"):
		return relativeTarget(api, rawPath)
	case strings.Contains(rawPath, ":"):
		return absoluteTarget(api, rawPath)
	}
	return nil, badPath()
}

func relativeTarget(api catalog.API, rawPath string) (*url.URL, error) {
	parsed, err := url.Parse(rawPath)
	if err != nil {
		return nil, badPath()
	}
	base, err := url.Parse(api.BaseURL)
	if err != nil || base.Host == "" {
		return nil, &RequestError{Code: "bad-provider", Message: "the provider's api.base_url is not a usable URL"}
	}
	return httpsURL(catalog.HostKey(base), parsed), nil
}

func absoluteTarget(api catalog.API, rawPath string) (*url.URL, error) {
	parsed, err := url.Parse(rawPath)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Opaque != "" || parsed.Host == "" {
		return nil, hostNotAllowed(api)
	}
	host := catalog.HostKey(parsed)
	if !slices.Contains(api.Hosts(), host) {
		return nil, hostNotAllowed(api)
	}
	return httpsURL(host, parsed), nil
}

func httpsURL(host string, from *url.URL) *url.URL {
	return &url.URL{Scheme: "https", Host: host, Path: from.Path, RawPath: from.RawPath, RawQuery: from.RawQuery}
}

func encodeQueryFlags(flags []string) (string, error) {
	pairs := make([]string, 0, len(flags))
	for _, flag := range flags {
		key, value, ok := strings.Cut(flag, "=")
		if !ok || key == "" {
			return "", &RequestError{Code: "bad-query", Message: "--query takes key=value"}
		}
		pairs = append(pairs, url.QueryEscape(key)+"="+url.QueryEscape(value))
	}
	return strings.Join(pairs, "&"), nil
}

func joinQuery(first, second string) string {
	if first == "" || second == "" {
		return first + second
	}
	return first + "&" + second
}

func isControlOrSpace(r rune) bool { return r <= ' ' || r == 0x7f }

func badPath() *RequestError {
	return &RequestError{Code: "bad-path", Message: "the path must start with / (e.g. /drive/v3/files) or be an https URL on an allowed host"}
}

func hostNotAllowed(api catalog.API) *RequestError {
	return &RequestError{Code: "host-not-allowed", Message: "the target must be a path or an https URL without userinfo on one of the provider's allowed hosts: " +
		strings.Join(api.Hosts(), ", ")}
}
