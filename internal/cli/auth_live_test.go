package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

type liveRow struct {
	Alias      string  `json:"alias"`
	Status     string  `json:"status"`
	Live       *bool   `json:"live"`
	LiveReason string  `json:"live_reason"`
	ExpiresAt  *string `json:"expires_at"`
}

func (c *callHarness) liveRows() []liveRow {
	c.t.Helper()
	return decodeJSON[[]liveRow](c.t, c.mustRun("auth", "check", "--live"))
}

func TestAuthCheckWithoutLiveIsUnchanged(t *testing.T) {
	c := newCallHarness(t)

	r := c.mustRun("auth", "check")

	var rows []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"live", "live_reason"} {
		if _, present := rows[0][key]; present {
			t.Errorf("auth check without --live reports %q", key)
		}
	}
	if c.transport.total() != 0 {
		t.Errorf("%d network requests without --live", c.transport.total())
	}
}

func TestAuthCheckLiveVerifiesTheTokenAtTheProvider(t *testing.T) {
	c := newCallHarness(t)

	rows := c.liveRows()

	if len(rows) != 1 || rows[0].Live == nil || !*rows[0].Live || rows[0].LiveReason != "ok" {
		t.Fatalf("rows = %+v, want live: true, reason ok", rows)
	}
	if c.google.refreshCount() != 0 {
		t.Error("a fresh token was refreshed")
	}
}

func TestAuthCheckLiveRefreshesAnExpiredTokenFirst(t *testing.T) {
	c := newCallHarness(t)
	c.expireToken()

	rows := c.liveRows()

	if rows[0].Live == nil || !*rows[0].Live || c.google.refreshCount() != 1 {
		t.Errorf("rows %+v, refreshes %d; want live after one refresh", rows, c.google.refreshCount())
	}
	if rows[0].ExpiresAt == nil || !strings.HasPrefix(*rows[0].ExpiresAt, "2026-11-20T11:") {
		t.Errorf("expires_at = %v, want the refreshed token's expiry", rows[0].ExpiresAt)
	}
}

func TestAuthCheckLiveReasons(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(c *callHarness)
		reason string
		status string
	}{
		{"revoked grant", func(c *callHarness) {
			c.google.configure(func(g *fakeGoogle) { g.refreshError = "invalid_grant" })
			c.expireToken()
		}, "needs-reauth", "needs-reauth"},
		{"token rejected", func(c *callHarness) { c.google.configure(func(g *fakeGoogle) { g.userinfoBad = true }) }, "token-rejected", "active"},
		{"provider unreachable", func(c *callHarness) { c.google.srv.Close() }, "network", "active"},
		{"refresh rejected", func(c *callHarness) {
			c.google.configure(func(g *fakeGoogle) { g.refreshError, g.refreshStatus = "server_error", 500 })
			c.expireToken()
		}, "refresh-rejected", "active"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCallHarness(t)
			tc.setup(c)

			r := c.mustRun("auth", "check", "--live")

			rows := decodeJSON[[]liveRow](t, r)
			if rows[0].Live == nil || *rows[0].Live || rows[0].LiveReason != tc.reason || rows[0].Status != tc.status {
				t.Errorf("row = %+v, want live: false, reason %s, status %s", rows[0], tc.reason, tc.status)
			}
			if strings.Contains(r.stdout+r.stderr, echoMarker) {
				t.Error("provider text reached the output")
			}
		})
	}
}

func TestAuthCheckLiveForANamedAccount(t *testing.T) {
	c := newCallHarness(t)

	r := c.mustRun("auth", "check", "google:work", "--live")

	if rows := decodeJSON[[]liveRow](t, r); len(rows) != 1 || rows[0].Live == nil || !*rows[0].Live {
		t.Errorf("rows = %+v", rows)
	}
}
