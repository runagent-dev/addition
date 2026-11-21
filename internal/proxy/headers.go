package proxy

import (
	"net/http"
	"net/textproto"
	"strings"

	"github.com/runagent-dev/addition/internal/vault"
)

// ownedHeaders are set by addn, or change what a request means to the server or a proxy.
var ownedHeaders = map[string]bool{
	"Authorization": true, "Host": true, "Cookie": true, "Content-Length": true, "Transfer-Encoding": true,
	"X-Http-Method-Override": true, "X-Method-Override": true, "X-Http-Method": true,
}

// ParseHeaders reads "Name: value" strings into a header set, refusing the headers addn owns.
func ParseHeaders(raw []string) (http.Header, error) {
	header := http.Header{}
	for _, entry := range raw {
		name, value, ok := strings.Cut(entry, ":")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || !isToken(name) || strings.ContainsFunc(value, isBadHeaderValueRune) {
			return nil, &RequestError{Code: "bad-header", Message: "-H takes 'Name: value' with a valid name and no control characters"}
		}
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if ownedHeaders[canonical] || strings.HasPrefix(canonical, "Proxy-") {
			return nil, &RequestError{Code: "forbidden-header", Message: "header " + canonical + " is set by addn and cannot be overridden"}
		}
		header.Add(canonical, value)
	}
	return header, nil
}

// ParseMethod upper-cases a method name and accepts only the ones addn can audit.
func ParseMethod(raw string) (string, error) {
	method := strings.ToUpper(raw)
	for _, known := range vault.CallMethods {
		if method == known {
			return method, nil
		}
	}
	return "", &RequestError{Code: "bad-method", Message: "the method must be one of " + strings.Join(vault.CallMethods, ", ")}
}

func isToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !letter && !strings.ContainsRune("!#$%&'*+-.^_`|~", r) {
			return false
		}
	}
	return true
}

func isBadHeaderValueRune(r rune) bool { return r != '\t' && (r < ' ' || r == 0x7f) }
