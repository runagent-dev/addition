package cli

import (
	"strings"
	"testing"
)

// useIdentityURL points Google's identity endpoint at rawURL.
func (c *callHarness) useIdentityURL(rawURL string) {
	c.t.Helper()
	c.deps.catalogFS = replaceInProvider(c.t, c.deps.catalogFS, c.google.srv.URL+"/userinfo", rawURL)
}

func TestAuthCheckLiveNeverSendsTheTokenToAnUnlistedIdentityURL(t *testing.T) {
	c := newCallHarness(t)
	c.expireToken()
	c.useIdentityURL("https://unlisted.example/userinfo")
	c.transport.reset()

	rows := c.liveRows()

	if rows[0].Live == nil || *rows[0].Live || rows[0].LiveReason != "identity-not-allowed" {
		t.Errorf("row = %+v, want live: false, reason identity-not-allowed", rows[0])
	}
	if c.google.refreshCount() != 0 || c.transport.total() != 0 {
		t.Errorf("refreshes %d, transport calls %d; the URL must be refused before anything is refreshed or sent", c.google.refreshCount(), c.transport.total())
	}
	if strings.Contains(c.lastStdout(), "unlisted.example") {
		t.Error("the refused URL was echoed")
	}
}
