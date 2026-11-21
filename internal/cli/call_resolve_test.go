package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/runagent-dev/addition/internal/vault"
)

func TestCallResolvesTheAccount(t *testing.T) {
	t.Run("the only account needs no -a", func(t *testing.T) {
		c := newCallHarness(t)

		r := c.mustRun("call", "GET", "/x")

		if len(c.api.requests()) != 1 || r.stdout == "" {
			t.Errorf("requests %d, stdout %q", len(c.api.requests()), r.stdout)
		}
	})
	t.Run("a service provider names its identity root", func(t *testing.T) {
		c := newCallHarness(t)

		c.mustRun("call", "--provider", "gmail", "-a", "work", "GET", "/x")
	})
	t.Run("several accounts need -a or a default", func(t *testing.T) {
		c := newCallHarness(t)
		c.google.configure(func(g *fakeGoogle) { g.identity, g.subject = "me@home.example", "2002" })
		c.connect("home", "gmail")
		c.transport.reset()

		r := c.call("GET", "/x")

		env := c.wantError(r, ExitUsage, "account-required")
		for _, alias := range []string{"work", "home"} {
			if !strings.Contains(env.Error.Message, alias) {
				t.Errorf("message %q does not list the alias %s", env.Error.Message, alias)
			}
		}
		if len(c.api.requests()) != 0 {
			t.Error("a request was sent without knowing the account")
		}

		c.mustRun("accounts", "default", "google:home")
		c.mustRun("call", "GET", "/x")
		c.mustRun("call", "-a", "work", "GET", "/x")
		if len(c.api.requests()) != 2 {
			t.Errorf("API requests = %d, want 2", len(c.api.requests()))
		}
	})
	t.Run("an unknown alias is not connected", func(t *testing.T) {
		c := newCallHarness(t)

		r := c.call("-a", "ghost", "GET", "/x")

		env := c.wantError(r, ExitNeedsConnect, "not-connected")
		if !strings.Contains(env.Error.Message, "addn connect") {
			t.Errorf("message %q does not say how to connect", env.Error.Message)
		}
	})
	t.Run("no accounts at all", func(t *testing.T) {
		h := newHarness(t)

		r := h.run("call", "GET", "/x")

		h.wantError(r, ExitNeedsConnect, "not-connected")
	})
	t.Run("an unknown provider is a usage error", func(t *testing.T) {
		c := newCallHarness(t)

		c.wantError(c.call("--provider", "nope", "-a", "work", "GET", "/x"), ExitUsage, "unknown-provider")
	})
	t.Run("an alias shared by two providers is ambiguous", func(t *testing.T) {
		c := newCallHarness(t)
		s, err := c.deps.openVault(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := s.CreateAccount(context.Background(), vault.Account{Provider: "github", Alias: "work"}); err != nil {
			t.Fatal(err)
		}

		c.wantError(c.call("-a", "work", "GET", "/x"), ExitUsage, "ambiguous-account")
		c.mustRun("call", "--provider", "google", "-a", "work", "GET", "/x")
		if len(c.api.requests()) != 1 {
			t.Errorf("API requests = %d, want 1", len(c.api.requests()))
		}
	})
}

func TestCallRefusesHostsOutsideTheAllowListBeforeSendingAnything(t *testing.T) {
	c := newCallHarness(t)
	c.expireToken()
	targets := []string{
		c.evil.url("/x"),
		"http://" + c.api.host() + "/x",
		"https://www.googleapis.com.evil.example/x",
		"https://evil.example/www.googleapis.com",
		"https://" + c.api.host() + "@" + c.evil.host() + "/x",
		"https://user:pw@" + c.api.host() + "/x",
		"https://" + c.api.host() + ":99/x",
		"//" + c.evil.host() + "/x",
		"//" + c.api.host() + "/x",
		"ftp://" + c.api.host() + "/x",
	}
	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			r := c.call("-a", "work", "GET", target)

			env := c.wantError(r, ExitUsage, "host-not-allowed")
			if r.stdout != "" || strings.Contains(env.Error.Message, "pw@") {
				t.Errorf("stdout %q, message %q", r.stdout, env.Error.Message)
			}
		})
	}
	if n := len(c.evil.requests()); n != 0 {
		t.Errorf("the disallowed server received %d requests", n)
	}
	if n := len(c.api.requests()); n != 0 {
		t.Errorf("the API received %d requests", n)
	}
	if n := c.transport.total(); n != 0 {
		t.Errorf("%d requests left addn; a refused target must not even refresh the token", n)
	}
	if n := c.google.refreshCount(); n != 0 {
		t.Errorf("the token endpoint was hit %d times", n)
	}
}

func TestCallAllowsAnAbsoluteURLOnAnAllowedHost(t *testing.T) {
	c := newCallHarness(t)

	c.mustRun("call", "-a", "work", "GET", c.api.url("/gmail/v1/users/me/profile?alt=json"), "--query", "fields=emailAddress")

	got := c.api.requests()[0]
	if got.Path != "/gmail/v1/users/me/profile" || got.RawQuery != "alt=json&fields=emailAddress" {
		t.Errorf("request = %+v", got)
	}
}

func TestCallRefusesHeadersAddnOwns(t *testing.T) {
	for _, h := range []string{
		"Authorization: Bearer stolen", "authorization:x", "Host: evil.example", "Cookie: session=COOKIESECRET",
		"Proxy-Authorization: x", "X-HTTP-Method-Override: DELETE", "Content-Length: 1",
	} {
		t.Run(h, func(t *testing.T) {
			c := newCallHarness(t)

			r := c.call("-a", "work", "POST", "/x", "-H", h)

			env := c.wantError(r, ExitUsage, "forbidden-header")
			if strings.Contains(r.stderr, "COOKIESECRET") || strings.Contains(r.stderr, "stolen") {
				t.Errorf("the error repeats the header value: %s", env.Error.Message)
			}
			if len(c.api.requests()) != 0 {
				t.Error("a request was sent")
			}
		})
	}
}

func TestCallRefusesAnAccountThatNeedsReauthWithoutSendingAnything(t *testing.T) {
	c := newCallHarness(t)
	c.google.configure(func(g *fakeGoogle) { g.refreshError = "invalid_grant" })
	c.expireToken()
	c.wantError(c.call("-a", "work", "GET", "/x"), ExitNeedsConnect, "needs-reauth")
	c.transport.reset()

	r := c.call("-a", "work", "GET", "/x")

	env := c.wantError(r, ExitNeedsConnect, "needs-reauth")
	if c.transport.total() != 0 || !strings.Contains(env.Error.Message, "addn connect google --as work") {
		t.Errorf("%d requests, message %q; want a local refusal that names `addn connect`", c.transport.total(), env.Error.Message)
	}
}
