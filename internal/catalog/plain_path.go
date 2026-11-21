package catalog

import (
	"net/url"
	"strings"
)

// maxDecodeRounds bounds how often a path segment is percent-decoded when looking for dot segments.
const maxDecodeRounds = 4

// PlainPath reports whether an escaped URL path is one a server cannot normalise into another
// route: it starts with /, has no empty, dot or ".." segment (also once percent-decoded, or with
// a ;parameter), and no backslash, encoded slash, control character or space.
func PlainPath(escaped string) bool {
	if !strings.HasPrefix(escaped, "/") || strings.Contains(escaped, "//") {
		return false
	}
	for _, segment := range strings.Split(escaped, "/") {
		if !plainSegment(segment) {
			return false
		}
	}
	return true
}

func plainSegment(segment string) bool {
	for range maxDecodeRounds {
		if strings.ContainsAny(segment, `/\`) || strings.ContainsFunc(segment, isControlOrSpace) || isDotSegment(segment) {
			return false
		}
		if !strings.Contains(segment, "%") {
			return true
		}
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			return false
		}
		segment = decoded
	}
	return false
}

func isDotSegment(segment string) bool {
	name, _, _ := strings.Cut(segment, ";")
	return name == "." || name == ".."
}

func isControlOrSpace(r rune) bool { return r <= ' ' || r == 0x7f }
