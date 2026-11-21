package cli

import (
	"context"
	"strings"
	"testing"
	"time"
)

// heldRefresh makes the provider hold each refresh request until released, after it has arrived.
type heldRefresh struct {
	arrived chan struct{}
	release chan struct{}
}

func holdRefreshes(c *callHarness) *heldRefresh {
	h := &heldRefresh{arrived: make(chan struct{}, 8), release: make(chan struct{})}
	c.google.configure(func(g *fakeGoogle) {
		g.rotateRefresh = true
		g.onToken = func() {
			h.arrived <- struct{}{}
			<-h.release
		}
	})
	return h
}

// cancelWhileHeld runs the call, cancels it once the provider holds its refresh, and lets the
// provider answer only after the command had every chance to stop on its own.
func (c *callHarness) cancelWhileHeld(h *heldRefresh, args ...string) result {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan result, 1)
	go func() { finished <- c.runContext(ctx, nil, append([]string{"call"}, args...)...) }()
	<-h.arrived
	cancel()
	var r result
	select {
	case r = <-finished:
		close(h.release)
	case <-time.After(300 * time.Millisecond):
		close(h.release)
		r = <-finished
	}
	return r
}

func TestCallKeepsARotatedRefreshTokenWhenCancelledDuringTheExchange(t *testing.T) {
	cases := map[string]func(c *callHarness) []string{
		"an expiring token": func(c *callHarness) []string {
			c.expireToken()
			return []string{"-a", "work", "GET", "/x"}
		},
		"a rejected token": func(c *callHarness) []string {
			c.api.respond(requireToken(refreshedAccessPrefix + "0001"))
			return []string{"-a", "work", "GET", "/x"}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			c := newCallHarness(t)
			args := setup(c)
			held := holdRefreshes(c)

			r := c.cancelWhileHeld(held, args...)

			c.wantError(r, ExitInternal, "interrupted")
			if strings.Contains(r.stdout, `"ok"`) {
				t.Errorf("stdout = %q; the command was cancelled", r.stdout)
			}
			c.google.configure(func(g *fakeGoogle) { g.onToken = nil })
			c.api.respond(jsonBody(`{"ok":true}`))
			c.expireToken()

			next := c.call("-a", "work", "GET", "/x")

			if next.code != ExitOK {
				t.Fatalf("the next call exit = %d: %s; the rotated refresh token was lost", next.code, next.stderr)
			}
			if got := c.google.refreshForm(1).Get("refresh_token"); !strings.HasPrefix(got, rotatedRefreshPrefix) {
				t.Errorf("the next refresh sent %q, want the rotated refresh token", got)
			}
			if status := c.wantAccounts(1)[0].Status; status != "active" {
				t.Errorf("account status = %q, want active", status)
			}
		})
	}
}

func TestCallDoesNotSendTheRequestAfterBeingCancelledWhileRefreshing(t *testing.T) {
	c := newCallHarness(t)
	c.expireToken()
	held := holdRefreshes(c)

	c.cancelWhileHeld(held, "-a", "work", "GET", "/x")

	if n := len(c.api.requests()); n != 0 {
		t.Errorf("the API saw %d requests; a cancelled call must not use the new token", n)
	}
}
