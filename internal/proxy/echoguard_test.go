package proxy_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/runagent-dev/addition/internal/proxy"
)

const echoToken = "ya29.echo-guard-secret-token"

func readAllGuarded(t *testing.T, g *proxy.EchoGuard, src io.Reader) (string, error) {
	t.Helper()
	got, err := io.ReadAll(g.Reader(src))
	return string(got), err
}

func TestEchoGuardPassesBodiesThatDoNotRepeatAToken(t *testing.T) {
	body := strings.Repeat("harmless filler ", 5000) + echoToken[:10]
	readers := map[string]func(io.Reader) io.Reader{
		"whole":    func(r io.Reader) io.Reader { return r },
		"one byte": iotest.OneByteReader,
		"half":     iotest.HalfReader,
	}
	for name, wrap := range readers {
		t.Run(name, func(t *testing.T) {
			got, err := readAllGuarded(t, proxy.NewEchoGuard([]string{echoToken}), wrap(strings.NewReader(body)))

			if err != nil || got != body {
				t.Errorf("got %d bytes, err %v; want the body untouched (a token prefix is not a token)", len(got), err)
			}
		})
	}
}

func TestEchoGuardStopsAtATokenAndEmitsNoPartOfIt(t *testing.T) {
	prefix := strings.Repeat("p", 3000)
	body := prefix + echoToken + strings.Repeat("s", 3000)
	readers := map[string]func(io.Reader) io.Reader{
		"whole":    func(r io.Reader) io.Reader { return r },
		"one byte": iotest.OneByteReader,
		"half":     iotest.HalfReader,
		"data+err": iotest.DataErrReader,
	}
	for name, wrap := range readers {
		t.Run(name, func(t *testing.T) {
			got, err := readAllGuarded(t, proxy.NewEchoGuard([]string{"other-token", echoToken}), wrap(strings.NewReader(body)))

			if !errors.Is(err, proxy.ErrCredentialEcho) {
				t.Fatalf("err = %v, want ErrCredentialEcho", err)
			}
			if strings.Contains(got, echoToken[:8]) || len(got) > len(prefix) {
				t.Errorf("emitted %d bytes holding token bytes; at most the %d bytes before the token may go out", len(got), len(prefix))
			}
		})
	}
}

func TestEchoGuardCatchesATokenSplitAcrossReads(t *testing.T) {
	for split := 1; split < len(echoToken); split++ {
		src := io.MultiReader(strings.NewReader("xx"+echoToken[:split]), strings.NewReader(echoToken[split:]+"yy"))

		got, err := readAllGuarded(t, proxy.NewEchoGuard([]string{echoToken}), src)

		if !errors.Is(err, proxy.ErrCredentialEcho) || len(got) > len("xx") {
			t.Fatalf("split at %d: got %q, err %v; want only the bytes before the token", split, got, err)
		}
	}
}

func TestEchoGuardKeepsFailingOnceItHasFailed(t *testing.T) {
	r := proxy.NewEchoGuard([]string{echoToken}).Reader(strings.NewReader(echoToken))
	buf := make([]byte, 64)
	if _, err := r.Read(buf); !errors.Is(err, proxy.ErrCredentialEcho) {
		t.Fatalf("first read err = %v", err)
	}
	if n, err := r.Read(buf); n != 0 || !errors.Is(err, proxy.ErrCredentialEcho) {
		t.Errorf("second read = %d, %v; want nothing more after a match", n, err)
	}
}

func TestEchoGuardIgnoresEmptySecretsAndChecksStrings(t *testing.T) {
	g := proxy.NewEchoGuard([]string{"", echoToken})

	got, err := readAllGuarded(t, g, bytes.NewReader([]byte("plain")))

	if err != nil || got != "plain" {
		t.Errorf("an empty secret must not match everything: %q, %v", got, err)
	}
	if !g.Leaks("text/plain; x=" + echoToken) {
		t.Error("Leaks missed a token inside a header value")
	}
	if g.Leaks("text/plain") {
		t.Error("Leaks flagged a clean header value")
	}
}
