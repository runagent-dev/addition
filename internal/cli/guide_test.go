package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runagent-dev/addition/internal/vault"
	"github.com/runagent-dev/addition/shared"
)

const maxAllGuideBytes = 32 * 1024

type guideEntry struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
	Command string `json:"command"`
}

func guideEntries(t *testing.T) []guideEntry {
	t.Helper()
	r := run("guide", "--json")
	if r.code != ExitOK {
		t.Fatalf("guide --json exit = %d, stderr %q", r.code, r.stderr)
	}
	var out struct {
		Topics []guideEntry `json:"topics"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatalf("guide --json is not JSON: %v\n%s", err, r.stdout)
	}
	return out.Topics
}

func TestGuideIndexPrintsOverviewAndTopics(t *testing.T) {
	r := run("guide")

	if r.code != ExitOK || r.stderr != "" {
		t.Fatalf("exit = %d, stderr = %q", r.code, r.stderr)
	}
	for _, want := range []string{"addition doctor", "## Topics", "addition guide connect", "addition guide all", "addition guide gmail"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("index lacks %q", want)
		}
	}
	if strings.HasPrefix(strings.TrimSpace(r.stdout), "{") {
		t.Error("index is JSON, want Markdown")
	}
}

func TestGuideJSONListsEveryTopic(t *testing.T) {
	topics := guideEntries(t)

	ids := map[string]guideEntry{}
	for _, e := range topics {
		ids[e.ID] = e
		if e.Summary == "" || e.Command != "addition guide "+e.ID {
			t.Errorf("entry %+v is malformed", e)
		}
	}
	for _, want := range []string{"overview", "connect", "call", "errors", "accounts", "apps", "troubleshooting", "gmail", "google-drive", "google-calendar", "google-chat", "google-docs", "all"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("topic %q is not listed", want)
		}
	}
}

func TestGuideTopicsPrint(t *testing.T) {
	for _, e := range guideEntries(t) {
		t.Run(e.ID, func(t *testing.T) {
			r := run("guide", e.ID)

			if r.code != ExitOK || r.stderr != "" {
				t.Fatalf("exit = %d, stderr = %q", r.code, r.stderr)
			}
			if !strings.HasPrefix(r.stdout, e.Summary) && e.ID != "all" {
				t.Errorf("topic does not start with its summary %q", e.Summary)
			}
			if len(strings.TrimSpace(r.stdout)) < 100 {
				t.Errorf("topic is nearly empty: %q", r.stdout)
			}
		})
	}
}

func TestGuideAllHoldsEveryTopicUnderTheSizeCap(t *testing.T) {
	r := run("guide", "all")

	if r.code != ExitOK {
		t.Fatalf("exit = %d", r.code)
	}
	if len(r.stdout) > maxAllGuideBytes {
		t.Errorf("guide all is %d bytes, cap %d", len(r.stdout), maxAllGuideBytes)
	}
	for _, e := range guideEntries(t) {
		if e.ID != "all" && !strings.Contains(r.stdout, e.Summary) {
			t.Errorf("guide all lacks topic %q", e.ID)
		}
	}
}

func TestGuideUnknownTopicListsTheValidOnes(t *testing.T) {
	r := run("guide", "nonsense")

	if r.code != ExitUsage || r.stdout != "" {
		t.Fatalf("exit = %d, stdout = %q", r.code, r.stdout)
	}
	env := decodeError(t, r.stderr)
	if env.Error.Code != "unknown-topic" {
		t.Errorf("code = %q", env.Error.Code)
	}
	for _, e := range guideEntries(t) {
		if !strings.Contains(env.Error.Message, e.ID) {
			t.Errorf("message does not name topic %q: %s", e.ID, env.Error.Message)
		}
	}
}

func TestGuideRejectsATopicWithJSON(t *testing.T) {
	if r := run("guide", "call", "--json"); r.code != ExitUsage {
		t.Errorf("exit = %d, want %d", r.code, ExitUsage)
	}
}

func TestGuideHelpSaysItIsNotJSON(t *testing.T) {
	r := run("guide", "--help")
	if !strings.Contains(r.stdout, "Markdown") || !strings.Contains(r.stdout, "--json") {
		t.Errorf("help does not explain the Markdown exception:\n%s", r.stdout)
	}
}

type noNetworkTransport struct{ t *testing.T }

func (f noNetworkTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("guide made a network request to %s", r.URL)
	return nil, http.ErrNotSupported
}

func TestGuideIsOfflineAndTouchesNoVault(t *testing.T) {
	var stdout, stderr bytes.Buffer
	d := deps{
		catalogFS:  shared.FS,
		guideFS:    shared.Guide,
		httpClient: &http.Client{Transport: noNetworkTransport{t}},
		env: vault.Env{LookupEnv: func(key string) (string, bool) {
			t.Errorf("guide read the environment variable %s", key)
			return "", false
		}},
	}

	for _, args := range [][]string{{"guide"}, {"guide", "--json"}, {"guide", "all"}, {"guide", "gmail"}} {
		if code := execute(args, &stdout, &stderr, d); code != ExitOK {
			t.Fatalf("%v: exit %d, stderr %q", args, code, stderr.String())
		}
	}
}

func TestGuideWorksWithoutADataDir(t *testing.T) {
	home := filepath.Join(t.TempDir(), "never-created")
	t.Setenv("ADDITION_HOME", home)

	if r := run("guide", "call"); r.code != ExitOK {
		t.Fatalf("exit = %d, stderr %q", r.code, r.stderr)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("the data dir exists: %v", err)
	}
}
