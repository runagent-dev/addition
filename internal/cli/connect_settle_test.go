package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/runagent-dev/addition/internal/faultpeer"
	"github.com/runagent-dev/addition/internal/oauth"
	"github.com/runagent-dev/addition/internal/vault"
)

// tokenTransport sends every request to the fake, except that fail picks, per token request,
// a failure to replace it with; nil sends it normally.
type tokenTransport struct {
	fail func(n int) failure
	n    atomic.Int32
}

type failure func(r *http.Request) (*http.Response, error)

func (tt *tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/token") {
		if fail := tt.fail(int(tt.n.Add(1))); fail != nil {
			return fail(r)
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (h *harness) failTokenRequests(fail func(n int) failure) {
	h.deps.httpClient = &http.Client{Transport: &tokenTransport{fail: fail}}
}

// failEchoing fails without touching the network, with an error that repeats the request.
func failEchoing(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	return nil, fmt.Errorf("dial tcp: %s %s", echoMarker, body)
}

// failAfterAnswer lets the fake answer, then reports a broken connection.
func failAfterAnswer(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return nil, fmt.Errorf("read: connection reset: %s %s", echoMarker, body)
}

// sendTo redirects the request to base, padded past what a fault peer reads before it resets.
func sendTo(base string) failure {
	return func(r *http.Request) (*http.Response, error) {
		target, err := url.Parse(base)
		if err != nil {
			return nil, err
		}
		padding := "&padding=" + strings.Repeat("x", 8<<20)
		out := r.Clone(r.Context())
		out.URL.Scheme, out.URL.Host, out.Host = target.Scheme, target.Host, ""
		out.Body = io.NopCloser(io.MultiReader(r.Body, strings.NewReader(padding)))
		out.ContentLength = r.ContentLength + int64(len(padding))
		out.GetBody = nil
		return http.DefaultTransport.RoundTrip(out)
	}
}

func refusedURL(t *testing.T) string {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}

func TestCompleteReleasesTheClaimWhenNoConnectionWasMade(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	refused := sendTo(refusedURL(t))
	h.failTokenRequests(func(n int) failure {
		if n == 1 {
			return refused
		}
		return nil
	})
	landed := h.google.consent(h.startTwoStep("gmail").AuthorizeURL)

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
	cases := map[string]func(t *testing.T) failure{
		"answered, then broken":         func(*testing.T) failure { return failAfterAnswer },
		"no evidence about the network": func(*testing.T) failure { return failEchoing },
		"reset on first byte":           func(t *testing.T) failure { return sendTo(faultpeer.Listen(t, faultpeer.ResetOnFirstByte)) },
		"reset mid-request":             func(t *testing.T) failure { return sendTo(faultpeer.Listen(t, faultpeer.ResetMidRequest)) },
		"reset after the whole request": func(t *testing.T) failure { return sendTo(faultpeer.Listen(t, faultpeer.ResetAfterRequest)) },
	}
	for name, makeFailure := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.addClient()
			fail := makeFailure(t)
			h.failTokenRequests(func(int) failure { return fail })
			landed := h.google.consent(h.startTwoStep("gmail").AuthorizeURL)

			env := h.wantError(h.run("connect", "--complete", landed), ExitUpstreamServer, "network")
			if !strings.Contains(env.Error.Message, "addition connect") {
				t.Errorf("message %q does not tell the user to connect again", env.Error.Message)
			}
			h.wantError(h.run("connect", "--complete", landed), ExitUsage, "unknown-state")
			h.wantAccounts(0)
		})
	}
}

func TestCompleteDropsTheAuthorizationWhenTheProviderAnswers5xx(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	h.google.tokenStatus = http.StatusBadGateway
	landed := h.google.consent(h.startTwoStep("gmail").AuthorizeURL)

	env := h.wantError(h.run("connect", "--complete", landed), ExitUpstreamServer, "token-exchange-rejected")
	if !strings.Contains(env.Error.Message, "addition connect") {
		t.Errorf("message %q does not tell the user to connect again", env.Error.Message)
	}
	h.wantError(h.run("connect", "--complete", landed), ExitUsage, "unknown-state")
}

// denialFor is the redirect a provider would send for landed's authorization had the user refused.
func denialFor(t *testing.T, landed string) string {
	u, err := url.Parse(landed)
	if err != nil {
		t.Fatal(err)
	}
	u.RawQuery = url.Values{"state": {u.Query().Get("state")}, "error": {"access_denied"}}.Encode()
	return u.String()
}

func TestDenialLeavesAClaimedAuthorizationAlone(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.google.onToken = func() {
		once.Do(func() { close(arrived) })
		<-release
	}
	landed := h.google.consent(h.startTwoStep("gmail").AuthorizeURL)

	completion := make(chan result, 1)
	go func() { completion <- h.run("connect", "--complete", landed) }()
	<-arrived
	denied := h.run("connect", "--complete", denialFor(t, landed))
	close(release)
	h.wantError(denied, ExitUsage, "authorization-in-progress")

	if r := <-completion; r.code != ExitOK {
		t.Fatalf("the held completion: exit %d, stderr %s", r.code, r.stderr)
	}
	h.wantAccounts(1)
}

func TestCompleteWhileAnotherCompletionIsInProgress(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	landed := h.google.consent(h.startTwoStep("gmail").AuthorizeURL)
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
		p := decodeJSON[pendingOut](t, h.mustRun("connect", "gmail", "--as", alias, "--no-browser"))
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
	h.mustRun("connect", "gmail", "--as", "work")
	firstURL := h.google.consent(h.startTwoStep("gmail").AuthorizeURL)
	secondURL := h.google.consent(h.startTwoStep("gmail").AuthorizeURL)

	h.google.grant = func(requested []string) []string { return append(slices.Clone(requested), scopeGmailLabels) }
	h.mustRun("connect", "--complete", firstURL)
	h.google.grant = nil
	env := h.wantError(h.run("connect", "--complete", secondURL), ExitUsage, "stale-authorization")
	if !strings.Contains(env.Error.Message, "addition connect") {
		t.Errorf("message %q does not tell the user to connect again", env.Error.Message)
	}

	show := decodeJSON[accountRow](t, h.mustRun("accounts", "show", "gmail:work"))
	if !containsAll(show.Scopes, scopeGmailLabels) {
		t.Errorf("stored scopes = %v, want the scope the first completion granted kept", show.Scopes)
	}
	h.wantError(h.run("connect", "--complete", secondURL), ExitUsage, "unknown-state")

	again := decodeJSON[connected](t, h.mustRun("connect", "gmail", "--as", "work"))
	if !containsAll(again.Scopes, append(gmailScopes, scopeGmailLabels)...) {
		t.Errorf("reconnect for the union = %v", again.Scopes)
	}
}

func TestProviderDiagnosticsNeverEchoWhatAdditionSent(t *testing.T) {
	t.Run("token exchange", func(t *testing.T) {
		h := newHarness(t)
		h.addClient()
		landed := h.google.consent(h.startTwoStep("gmail").AuthorizeURL)
		h.google.echo = true
		h.wantError(h.run("connect", "--complete", landed), ExitUpstreamClient, "token-exchange-rejected")
	})
	t.Run("token transport", func(t *testing.T) {
		h := newHarness(t)
		h.addClient()
		h.failTokenRequests(func(int) failure { return failEchoing })
		h.wantError(h.run("connect", "gmail", "--as", "work"), ExitUpstreamServer, "network")
	})
	t.Run("userinfo", func(t *testing.T) {
		h := newHarness(t)
		h.addClient()
		h.google.userinfoBad = true
		env := h.wantError(h.run("connect", "gmail", "--as", "work"), ExitUpstreamClient, "identity-rejected")
		if !strings.Contains(env.Error.Message, "HTTP 401") || !strings.Contains(env.Error.Message, "invalid_request") {
			t.Errorf("message %q lacks the status or the allowlisted code", env.Error.Message)
		}
	})
	t.Run("revoke", func(t *testing.T) {
		h := newHarness(t)
		h.addClient()
		h.connect("work", "gmail")
		h.google.echo = true
		out := decodeJSON[removed](t, h.mustRun("accounts", "remove", "gmail:work", "--revoke"))
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
			h.wantError(h.run("connect", "gmail", "--as", "work"), ExitUpstreamClient, code)
			landed := h.google.consent(h.startTwoStep("gmail").AuthorizeURL)
			h.wantError(h.run("connect", "--complete", landed), ExitUpstreamClient, code)
		}
	})
}
