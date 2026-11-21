package cli

import (
	"bytes"
	"strings"
	"testing"
)

// withoutFakeHost drops the fake provider's host from gmail's allowed hosts, so the identity
// URL, which lives there, is no longer on the app's list.
func (h *harness) withoutFakeHost() {
	h.t.Helper()
	host := strings.TrimPrefix(h.google.srv.URL, "http://")
	h.deps.catalogFS = editProvider(h.t, h.deps.catalogFS, "gmail", func(data []byte) []byte {
		return bytes.ReplaceAll(data, []byte("\n    - \""+host+"\""), nil)
	})
}

func TestConnectRefusesAnIdentityURLOutsideTheAppsHostsBeforeAnyRequest(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.withoutFakeHost()
	browsed := false
	h.deps.openBrowser = func(string) <-chan error { browsed = true; return nil }

	h.wantError(h.run("connect", "gmail", "--as", "work"), ExitUsage, "identity-not-allowed")
	h.wantError(h.run("connect", "gmail", "--as", "work", "--no-browser"), ExitUsage, "identity-not-allowed")

	if browsed || len(h.google.authorizes) != 0 || len(h.google.exchanges) != 0 {
		t.Errorf("browser opened %v, %d authorizations, %d exchanges; nothing may start", browsed, len(h.google.authorizes), len(h.google.exchanges))
	}
	h.wantAccounts(0)
}

func TestCompleteRefusesAnIdentityURLOutsideTheAppsHostsAndKeepsTheAuthorization(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	pending := h.startTwoStep("gmail")
	redirect := h.google.consent(pending.AuthorizeURL)
	good := h.deps.catalogFS
	h.withoutFakeHost()

	h.wantError(h.run("connect", "--complete", redirect), ExitUsage, "identity-not-allowed")

	if len(h.google.exchanges) != 0 {
		t.Errorf("%d token exchanges; the code must not be redeemed for a token that could not be used", len(h.google.exchanges))
	}
	h.wantAccounts(0)
	h.deps.catalogFS = good
	h.mustRun("connect", "--complete", redirect)
	h.wantAccounts(1)
}
