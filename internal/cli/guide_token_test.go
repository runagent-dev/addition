package cli

import (
	"strings"
	"testing"
)

func TestTheGuideTeachesTheTokenFlow(t *testing.T) {
	g, _ := loadedGuide(t)
	want := map[string][]string{
		"connect":         {"--token-stdin", "--token-env", "--method", "method-not-supported", "api_key"},
		"accounts":        {"api_key", "method", "scopes_ready"},
		"errors":          {"invalid-token", "token-required", "empty-token", "bad-token", "token-env-unset", "token-prefix-mismatch"},
		"troubleshooting": {"invalid-token", "--token-stdin"},
	}
	for id, fragments := range want {
		topic, ok := g.Topic(id)
		if !ok {
			t.Fatalf("no %s topic", id)
		}
		for _, fragment := range fragments {
			if !strings.Contains(topic.Body, fragment) {
				t.Errorf("the %s topic does not mention %q", id, fragment)
			}
		}
	}
}

func TestTheGuideNeverShowsATokenOnACommandLine(t *testing.T) {
	g, _ := loadedGuide(t)
	for _, topic := range g.Topics() {
		for _, line := range guideCommands(topic.Body) {
			if !strings.HasPrefix(line, "addition connect") {
				continue
			}
			for _, word := range shellWords(line) {
				if strings.HasPrefix(word, "pk_") || strings.HasPrefix(word, "ghp_") || strings.HasPrefix(word, "github_pat_") {
					t.Errorf("%s: %q puts a token in the arguments", topic.ID, line)
				}
			}
		}
	}
}

func TestTheGuideCommandCheckerKnowsTheTokenFlags(t *testing.T) {
	_, c := loadedGuide(t)
	root := newRoot(deps{})
	for _, line := range []string{
		`addition connect github --as me --token-stdin`,
		`addition connect github --as me --token-env GITHUB_TOKEN`,
		`addition connect github --as me --method oauth2 --no-browser`,
	} {
		if problem := checkCommandLine(root, c, nil, line); problem != "" {
			t.Errorf("%q: %s", line, problem)
		}
	}
	for _, line := range []string{`addition connect github --as me --token ghp_x`, `addition connect github --as me --api-key x`} {
		if checkCommandLine(root, c, nil, line) == "" {
			t.Errorf("%q passed the checker", line)
		}
	}
}
