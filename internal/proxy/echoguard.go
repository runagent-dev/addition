package proxy

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
)

// ErrCredentialEcho is a response that repeated an access token addition sent.
var ErrCredentialEcho = errors.New("the response repeated an access token")

const guardChunk = 32 << 10

// EchoGuard stops response data from carrying the access tokens that were sent to get it.
type EchoGuard struct {
	secrets [][]byte
	// holdBack is how many trailing bytes a reader keeps, so a token split across reads is seen whole before any of it is released.
	holdBack int
}

// NewEchoGuard guards against the given tokens; empty ones are ignored.
func NewEchoGuard(secrets []string) *EchoGuard {
	g := &EchoGuard{}
	for _, secret := range secrets {
		if secret != "" {
			g.secrets = append(g.secrets, []byte(secret))
			g.holdBack = max(g.holdBack, len(secret)-1)
		}
	}
	return g
}

// Leaks reports whether s, such as a header value, contains a guarded token.
func (g *EchoGuard) Leaks(s string) bool {
	return slices.ContainsFunc(g.secrets, func(secret []byte) bool { return strings.Contains(s, string(secret)) })
}

// Lookahead is how many bytes past a limit a reader must still inspect to catch a token that straddles it.
func (g *EchoGuard) Lookahead() int { return g.holdBack }

// Reader streams src, withholding the last bytes read until they can no longer start a token.
// Once a token appears it returns ErrCredentialEcho and releases nothing more.
func (g *EchoGuard) Reader(src io.Reader) io.Reader {
	return &guardedReader{guard: g, src: src}
}

type guardedReader struct {
	guard      *EchoGuard
	src        io.Reader
	buf        []byte
	releasable int
	err        error
}

func (r *guardedReader) Read(p []byte) (int, error) {
	for {
		if r.releasable > 0 {
			n := copy(p, r.buf[:min(len(p), r.releasable)])
			r.buf, r.releasable = r.buf[n:], r.releasable-n
			return n, nil
		}
		if r.err != nil {
			return 0, r.err
		}
		r.fill()
	}
}

func (r *guardedReader) fill() {
	chunk := make([]byte, guardChunk)
	n, err := r.src.Read(chunk)
	r.buf = append(r.buf, chunk[:n]...)
	switch {
	case r.guard.found(r.buf):
		r.buf, r.releasable, r.err = nil, 0, ErrCredentialEcho
	case err != nil:
		r.releasable, r.err = len(r.buf), err
	default:
		r.releasable = max(len(r.buf)-r.guard.holdBack, 0)
	}
}

func (g *EchoGuard) found(data []byte) bool {
	return slices.ContainsFunc(g.secrets, func(secret []byte) bool { return bytes.Contains(data, secret) })
}
