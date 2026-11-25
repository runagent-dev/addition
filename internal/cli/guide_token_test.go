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

func TestTheGuideSaysWhatA401Does(t *testing.T) {
	g, _ := loadedGuide(t)
	call, _ := g.Topic("call")
	var line string
	for l := range strings.SplitSeq(call.Body, "\n") {
		if strings.Contains(l, "retries once on a 401") || strings.Contains(l, "401") && strings.Contains(l, "renews") {
			line = l
		}
	}
	for _, want := range []string{"refresh token", "upstream-error", "needs-reauth", "empty stdout"} {
		if !strings.Contains(line, want) {
			t.Errorf("the 401 line %q does not say %q: a 401 with a refresh token is retried once and, if it repeats, is the upstream body; one with none, such as a token account, exits 3 with nothing on stdout", line, want)
		}
	}
}

func TestTheOAuthCompletionExampleListsTheMethodField(t *testing.T) {
	g, _ := loadedGuide(t)
	connect, _ := g.Topic("connect")
	var line string
	for l := range strings.SplitSeq(connect.Body, "\n") {
		if strings.Contains(l, `"provider","alias","identity","scopes","created"`) && strings.Contains(l, "read -r URL") {
			line = l
		}
	}
	if !strings.Contains(line, `"method"`) {
		t.Errorf("the OAuth completion line %q must list the method field connect now prints", line)
	}
}

func TestTheGuideDescribesATokenAsGitHubsDefaultAndShowsHowToKeepOAuth(t *testing.T) {
	g, _ := loadedGuide(t)
	apps, _ := g.Topic("apps")
	var line string
	for l := range strings.SplitSeq(apps.Body, "\n") {
		if strings.Contains(l, "`github`") {
			line = l
		}
	}
	if strings.Contains(line, "not OAuth") || !strings.Contains(line, "default") || !strings.Contains(line, "--method oauth2") {
		t.Errorf("the apps line %q must say a token is the default and how to use OAuth", line)
	}
	accounts, _ := g.Topic("accounts")
	if !strings.Contains(accounts.Body, "--method oauth2") {
		t.Error("accounts.md must show --method oauth2 for reconnecting an OAuth account of a token-default app")
	}
}
