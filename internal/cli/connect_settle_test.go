package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/runagent-dev/addition/internal/oauth"
	"github.com/runagent-dev/addition/internal/vault"
)

// tokenTransport sends every request to the fake, except that fail decides, per token request,
// whether to fail it before it is sent, after the fake has answered, or not at all.
type tokenTransport struct {
	fail func(n int) failure
	n    atomic.Int32
}

type failure int

const (
	noFailure failure = iota
	failBeforeSending
	failAfterAnswer
)

func (tt *tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(r.URL.Path, "/token") {
		return http.DefaultTransport.RoundTrip(r)
	}
	switch tt.fail(int(tt.n.Add(1))) {
	case failBeforeSending:
		body, _ := io.ReadAll(r.Body)
		return nil, fmt.Errorf("dial tcp: %s %s", echoMarker, body)
	case failAfterAnswer:
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("read: connection reset: %s %s", echoMarker, body)
		}
		return nil, err
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (h *harness) failTokenRequests(fail func(n int) failure) {
	h.deps.httpClient = &http.Client{Transport: &tokenTransport{fail: fail}}
}

func TestCompleteReleasesTheClaimWhenTheExchangeWasNeverSent(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.failTokenRequests(func(n int) failure {
		if n == 1 {
			return failBeforeSending
		}
		return noFailure
	})
	landed := h.google.consent(h.startTwoStep().AuthorizeURL)

	env := h.wantError(h.run("connect", "--complete", landed), ExitUpstreamServer, "network")
	if env.Error.Message != "network error contacting the token endpoint" {
		t.Errorf("message = %q", env.Error.Message)
	}
	got := decodeJSON[connected](t, h.mustRun("connect", "--complete", landed))
	if got.Identity != workIdentity || len(h.google.exchanges) != 1 {
		t.Errorf("retry = %+v after %d exchanges", got, len(h.google.exchanges))
	}
}

func TestCompleteDropsTheAuthorizationWhenTheExchangeOutcomeIsUnknown(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.failTokenRequests(func(int) failure { return failAfterAnswer })
	landed := h.google.consent(h.startTwoStep().AuthorizeURL)

	env := h.wantError(h.run("connect", "--complete", landed), ExitUpstreamServer, "network")
	if !strings.Contains(env.Error.Message, "addn connect") {
		t.Errorf("message %q does not tell the user to connect again", env.Error.Message)
	}
	h.wantError(h.run("connect", "--complete", landed), ExitUsage, "unknown-state")
	if len(h.google.exchanges) != 1 {
		t.Errorf("token endpoint hit %d times, want once", len(h.google.exchanges))
	}
}

func TestCompleteDropsTheAuthorizationWhenTheProviderAnswers5xx(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.google.tokenStatus = http.StatusBadGateway
	landed := h.google.consent(h.startTwoStep().AuthorizeURL)

	env := h.wantError(h.run("connect", "--complete", landed), ExitUpstreamServer, "token-exchange-rejected")
	if !strings.Contains(env.Error.Message, "addn connect") {
		t.Errorf("message %q does not tell the user to connect again", env.Error.Message)
	}
	h.wantError(h.run("connect", "--complete", landed), ExitUsage, "unknown-state")
}

func TestCompleteWhileAnotherCompletionIsInProgress(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	landed := h.google.consent(h.startTwoStep().AuthorizeURL)
	cb, err := oauth.ParseCallback(landed)
	if err != nil {
		t.Fatal(err)
	}
	s, err := vault.Open(context.Background(), h.deps.env)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ClaimPendingAuth(context.Background(), cb.State); err != nil {
		t.Fatal(err)
	}

	h.wantError(h.run("connect", "--complete", landed), ExitUsage, "authorization-in-progress")
	if len(h.google.exchanges) != 0 {
		t.Error("a claimed authorization reached the token endpoint again")
	}
}

func TestConcurrentCompletionsExchangeOnce(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	for round := range 5 {
		alias := fmt.Sprintf("work%d", round)
		p := decodeJSON[pendingOut](t, h.mustRun("connect", "google", "--as", alias, "--no-browser", "--services", "gmail"))
		landed := h.google.consent(p.AuthorizeURL)
		before := len(h.google.exchanges)

		results := make([]result, 2)
		var wg sync.WaitGroup
		for i := range results {
			wg.Go(func() { results[i] = h.run("connect", "--complete", landed) })
		}
		wg.Wait()

		won := 0
		for _, r := range results {
			if r.code == ExitOK {
				won++
				continue
			}
			if env := decodeError(t, lastLine(r.stderr)); env.Error.Code != "authorization-in-progress" && env.Error.Code != "unknown-state" {
				t.Errorf("round %d: loser got %q", round, env.Error.Code)
			}
		}
		if won != 1 || len(h.google.exchanges)-before != 1 {
			t.Errorf("round %d: %d successes and %d exchanges, want one of each", round, won, len(h.google.exchanges)-before)
		}
	}
}

func TestOverlappingReconsentsRefuseAStaleCompletion(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.mustRun("connect", "google", "--as", "work", "--services", "gmail")
	driveURL := h.google.consent(h.startTwoStep("--services", "drive").AuthorizeURL)
	calendarURL := h.google.consent(h.startTwoStep("--services", "calendar").AuthorizeURL)

	h.mustRun("connect", "--complete", driveURL)
	env := h.wantError(h.run("connect", "--complete", calendarURL), ExitUsage, "stale-authorization")
	if !strings.Contains(env.Error.Message, "addn connect") {
		t.Errorf("message %q does not tell the user to connect again", env.Error.Message)
	}

	show := decodeJSON[accountRow](t, h.mustRun("accounts", "show", "google:work"))
	if !containsAll(show.Scopes, scopeDrive) || slices.Contains(show.Scopes, scopeCalendar) {
		t.Errorf("stored scopes = %v, want drive kept and calendar not yet added", show.Scopes)
	}
	h.wantError(h.run("connect", "--complete", calendarURL), ExitUsage, "unknown-state")

	again := decodeJSON[connected](t, h.mustRun("connect", "google", "--as", "work", "--services", "calendar"))
	if !containsAll(again.Scopes, scopeDrive, scopeCalendar) {
		t.Errorf("reconnect for the union = %v", again.Scopes)
	}
}

func TestProviderDiagnosticsNeverEchoWhatAddnSent(t *testing.T) {
	t.Run("token exchange", func(t *testing.T) {
		h := newHarness(t)
		h.addClient()
		landed := h.google.consent(h.startTwoStep().AuthorizeURL)
		h.google.echo = true
		h.wantError(h.run("connect", "--complete", landed), ExitUpstreamClient, "token-exchange-rejected")
	})
	t.Run("token transport", func(t *testing.T) {
		h := newHarness(t)
		h.addClient()
		h.failTokenRequests(func(int) failure { return failBeforeSending })
		h.wantError(h.run("connect", "google", "--as", "work"), ExitUpstreamServer, "network")
	})
	t.Run("userinfo", func(t *testing.T) {
		h := newHarness(t)
		h.addClient()
		h.google.userinfoBad = true
		env := h.wantError(h.run("connect", "google", "--as", "work"), ExitUpstreamClient, "identity-rejected")
		if !strings.Contains(env.Error.Message, "HTTP 401") || !strings.Contains(env.Error.Message, "invalid_request") {
			t.Errorf("message %q lacks the status or the allowlisted code", env.Error.Message)
		}
	})
	t.Run("revoke", func(t *testing.T) {
		h := newHarness(t)
		h.addClient()
		h.connect("work", "gmail")
		h.google.echo = true
		out := decodeJSON[removed](t, h.mustRun("accounts", "remove", "google:work", "--revoke"))
		if out.Revoke.OK || !strings.Contains(out.Revoke.Error, "HTTP 400") {
			t.Errorf("revoke = %+v", out.Revoke)
		}
	})
	t.Run("callback", func(t *testing.T) {
		for _, echo := range []bool{false, true} {
			h := newHarness(t)
			h.addClient()
			h.google.deny, h.google.echo = true, echo
			code := "access-denied"
			if echo {
				code = "authorization-error"
			}
			h.wantError(h.run("connect", "google", "--as", "work"), ExitUpstreamClient, code)
			landed := h.google.consent(h.startTwoStep().AuthorizeURL)
			h.wantError(h.run("connect", "--complete", landed), ExitUpstreamClient, code)
		}
	})
}
