package proxy_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/runagent-dev/addition/internal/proxy"
)

func TestParseHeadersPassesOrdinaryHeaders(t *testing.T) {
	got, err := proxy.ParseHeaders([]string{
		"Content-Type: application/json", "x-goog-user-project:my-project", "Accept:  text/plain ", "X-Multi: a", "x-multi: b",
	})

	if err != nil {
		t.Fatalf("ParseHeaders: %v", err)
	}
	want := http.Header{
		"Content-Type": {"application/json"}, "X-Goog-User-Project": {"my-project"}, "Accept": {"text/plain"}, "X-Multi": {"a", "b"},
	}
	for k, v := range want {
		if strings.Join(got[k], ",") != strings.Join(v, ",") {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestParseHeadersRefusesTheOnesAddnOwns(t *testing.T) {
	for _, h := range []string{
		"Authorization: Bearer stolen", "authorization: x", " AUTHORIZATION : x", "Host: evil.example", "Cookie: session=secret",
		"Proxy-Authorization: x", "proxy-connection: keep-alive", "Proxy-Anything: x", "X-HTTP-Method-Override: DELETE",
		"x-method-override: DELETE", "X-HTTP-Method: DELETE", "Content-Length: 5", "Transfer-Encoding: chunked",
	} {
		t.Run(h, func(t *testing.T) {
			_, err := proxy.ParseHeaders([]string{h})

			if requestErrorCode(err) != "forbidden-header" {
				t.Fatalf("err = %v, want code forbidden-header", err)
			}
			if _, value, _ := strings.Cut(h, ":"); strings.Contains(err.Error(), strings.TrimSpace(value)) && len(strings.TrimSpace(value)) > 3 {
				t.Errorf("the error repeats the header value: %v", err)
			}
		})
	}
}

func TestParseHeadersRefusesMalformedHeaders(t *testing.T) {
	for _, h := range []string{"NoColon", ": value", "Bad Name: x", "Bad(Name): x", "X-A: line\nbreak", "X-A: cr\rhere", "X-A: nul\x00", "Név: x"} {
		t.Run(h, func(t *testing.T) {
			if _, err := proxy.ParseHeaders([]string{h}); requestErrorCode(err) != "bad-header" {
				t.Errorf("err = %v, want code bad-header", err)
			}
		})
	}
}

func TestParseHeadersChecksRawControlCharactersBeforeTrimming(t *testing.T) {
	for _, h := range []string{"X-Test: value\r\n", "X-Test: value\n", "X-Test: value\r", "X-Test:\r\n value", "X-Test: v\x0b", "X-Test: v\x1b[0m\n"} {
		t.Run(strings.NewReplacer("\r", `\r`, "\n", `\n`, "\x0b", `\v`, "\x1b", `\e`).Replace(h), func(t *testing.T) {
			if _, err := proxy.ParseHeaders([]string{h}); requestErrorCode(err) != "bad-header" {
				t.Errorf("err = %v, want code bad-header", err)
			}
		})
	}
}

func TestParseHeadersAllowsTabPadding(t *testing.T) {
	got, err := proxy.ParseHeaders([]string{"X-Test:\tvalue\t", "X-Inner: a\tb"})

	if err != nil || got.Get("X-Test") != "value" || got.Get("X-Inner") != "a\tb" {
		t.Errorf("got %v, %v; tabs are ordinary header whitespace", got, err)
	}
}

func TestParseHeadersRefusesHopByHopAndUpgradeHeaders(t *testing.T) {
	for _, h := range []string{"Upgrade: websocket", "upgrade: h2c", "Connection: upgrade", "connection: close", "TE: trailers", "te: gzip", "Trailer: X-Foo", "Keep-Alive: timeout=5", "keep-alive: 1", "Accept-Encoding: gzip", "accept-encoding: br", "Accept-Encoding: identity"} {
		t.Run(h, func(t *testing.T) {
			if _, err := proxy.ParseHeaders([]string{h}); requestErrorCode(err) != "forbidden-header" {
				t.Errorf("err = %v, want code forbidden-header", err)
			}
		})
	}
}
