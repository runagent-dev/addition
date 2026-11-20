package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type clientAdded struct {
	Provider string `json:"provider"`
	ClientID string `json:"client_id"`
	Replaced bool   `json:"replaced"`
}

type clientRow struct {
	Provider  string `json:"provider"`
	ClientID  string `json:"client_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func TestClientAddAndList(t *testing.T) {
	h := newHarness(t)
	file := writeClientFile(t, "installed")

	first := decodeJSON[clientAdded](t, h.mustRun("client", "add", "google", "--file", file))
	if first != (clientAdded{Provider: "google", ClientID: fakeClientID, Replaced: false}) {
		t.Errorf("first add = %+v", first)
	}
	second := decodeJSON[clientAdded](t, h.mustRun("client", "add", "google", "--file", file))
	if !second.Replaced {
		t.Errorf("re-adding did not say it replaced the client: %+v", second)
	}

	r := h.mustRun("client", "list")
	rows := decodeJSON[[]clientRow](t, r)
	if len(rows) != 1 || rows[0].Provider != "google" || rows[0].ClientID != fakeClientID || rows[0].CreatedAt == "" {
		t.Errorf("client list = %+v", rows)
	}
	if strings.Contains(r.stdout, "secret") {
		t.Errorf("client list mentions a secret: %s", r.stdout)
	}
}

func TestClientListIsAnEmptyArray(t *testing.T) {
	h := newHarness(t)
	if r := h.mustRun("client", "list"); strings.TrimSpace(r.stdout) != "[]" {
		t.Errorf("stdout = %q, want []", r.stdout)
	}
}

func TestClientAddRejectsAWebClient(t *testing.T) {
	h := newHarness(t)
	env := h.wantError(h.run("client", "add", "google", "--file", writeClientFile(t, "web")), ExitUsage, "web-client")
	if !strings.Contains(env.Error.Message, "Desktop app") {
		t.Errorf("message %q does not say to create a Desktop app client", env.Error.Message)
	}
}

func TestClientAddRejectsBadInput(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := writeClientFile(t, "installed")
	cases := map[string]struct {
		args []string
		code string
	}{
		"unknown provider":     {[]string{"client", "add", "nope", "--file", good}, "unknown-provider"},
		"not an identity root": {[]string{"client", "add", "gmail", "--file", good}, "not-identity-root"},
		"missing file":         {[]string{"client", "add", "google", "--file", filepath.Join(dir, "absent.json")}, "bad-client-file"},
		"not JSON":             {[]string{"client", "add", "google", "--file", write("junk.json", "{nope")}, "bad-client-file"},
		"no installed block":   {[]string{"client", "add", "google", "--file", write("other.json", `{"other":{}}`)}, "bad-client-file"},
		"no secret":            {[]string{"client", "add", "google", "--file", write("nosecret.json", `{"installed":{"client_id":"x"}}`)}, "bad-client-file"},
		"no --file":            {[]string{"client", "add", "google"}, "usage"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h.wantError(h.run(tc.args...), ExitUsage, tc.code)
		})
	}
}
