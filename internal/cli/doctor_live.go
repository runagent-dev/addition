package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/oauth"
	"github.com/runagent-dev/addition/internal/proxy"
	"github.com/runagent-dev/addition/internal/vault"
)

const (
	probeAPIDisabled = "api-disabled"
	probeForbidden   = "forbidden"
	probeUnavailable = "unavailable"

	reasonServiceDisabled = "SERVICE_DISABLED"
	maxProbeErrorBody     = 64 << 10
)

const defaultProbeTimeout = 15 * time.Second

// probeTimeout bounds one probe request and its error body. A token refresh the probe starts is not
// cut short (CLAUDE.md §6.6: once the exchange starts, it and its commit finish), so a probe may take
// up to probeTimeout plus the token endpoint's own timeout.
var probeTimeout = defaultProbeTimeout

// liveChecks probes each healthy account's app once; the rest are skipped.
func (a *accountSession) liveChecks(cmd *cobra.Command, d deps, accts checkedAccounts) ([]doctorCheck, error) {
	var checks []doctorCheck
	for _, acct := range accts.all {
		id := "live:" + acct.Provider + ":" + acct.Alias
		provider, found := a.catalog.Provider(acct.Provider)
		switch {
		case !slices.ContainsFunc(accts.healthy, func(h vault.Account) bool { return h.ID == acct.ID }) || !found:
			checks = append(checks, doctorCheck{ID: id, Status: checkSkip, Summary: "not probed: the account needs attention first"})
		case provider.API.ProbePath == "":
			checks = append(checks, doctorCheck{ID: id, Status: checkSkip, Summary: "this app defines no probe request"})
		default:
			check, err := a.probe(cmd, d, provider, acct)
			if err != nil {
				return nil, err
			}
			check.ID = id
			checks = append(checks, check)
		}
	}
	return checks, nil
}

// probe sends the app's probe request through the same path as `addition call` and classifies the answer.
func (a *accountSession) probe(cmd *cobra.Command, d deps, provider *catalog.Provider, acct vault.Account) (doctorCheck, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), probeTimeout)
	defer cancel()
	target, err := proxy.ResolveURL(provider.API, provider.API.ProbePath, nil)
	if err != nil {
		return doctorCheck{}, callRequestError(err)
	}
	held := &heldValues{}
	collect := func() { held.collect(context.WithoutCancel(ctx), a, acct, provider) }
	collect()
	tokens := &proxy.TokenManager{Store: a.store, Client: d.httpClient, Provider: provider, Account: acct, Now: d.now}
	token, err := tokens.Access(ctx)
	collect()
	if err != nil {
		return probeFailure(acct, err)
	}
	engine := &proxy.Engine{Client: d.httpClient, API: provider.API, AllowCleartext: d.loopbackProviders, Renewer: collectingRenewer{tokens: tokens, collect: collect}, Now: d.now, Sleep: d.sleep}
	header := http.Header{"User-Agent": {"addition/" + currentBuild().Version}}
	resp, err := engine.Do(ctx, proxy.Request{Method: http.MethodGet, URL: target, Header: header}, token)
	if err != nil {
		if attempted, sent := errors.AsType[*proxy.Attempted](err); sent {
			recordCall(cmd, a.store, acct, http.MethodGet, attempted.Status)
		}
		return probeFailure(acct, err)
	}
	defer resp.Body.Close()
	recordCall(cmd, a.store, acct, http.MethodGet, resp.Status)
	return probeVerdict(acct, provider.API, resp, *held), nil
}

// collectingRenewer notes the vault's secrets after every renewal, so each refresh token a probe rotates through is held.
type collectingRenewer struct {
	tokens  *proxy.TokenManager
	collect func()
}

func (r collectingRenewer) Renew(ctx context.Context, rejected string) (string, error) {
	token, err := r.tokens.Renew(ctx, rejected)
	r.collect()
	return token, err
}

// heldValues are what a probe's response must never repeat: every refresh and access token the account
// held at any point of the probe, and the OAuth client secret.
type heldValues struct {
	secrets  []string
	clientID string
}

func (h *heldValues) collect(ctx context.Context, a *accountSession, acct vault.Account, provider *catalog.Provider) {
	if cred, found, err := a.credential(ctx, acct); err == nil && found {
		h.secrets = append(h.secrets, nonEmpty(cred.RefreshToken, cred.AccessToken)...)
	}
	if client, err := a.store.GetOAuthClient(ctx, provider.IdentityRoot); err == nil {
		h.secrets = append(h.secrets, nonEmpty(client.ClientSecret)...)
		h.clientID = client.ClientID
	}
}

func nonEmpty(values ...string) []string {
	return slices.DeleteFunc(slices.Clone(values), func(v string) bool { return v == "" })
}

func probeVerdict(acct vault.Account, api catalog.API, resp *proxy.Response, held heldValues) doctorCheck {
	switch {
	case resp.Status >= 200 && resp.Status <= 299:
		return doctorCheck{Status: checkOK, Code: checkOK, Summary: fmt.Sprintf("the API accepted the account's token (HTTP %d)", resp.Status)}
	case resp.Status == http.StatusUnauthorized:
		return probeNeedsReauth(acct, "the API rejected the account's token (HTTP 401)")
	case resp.Status == http.StatusForbidden:
		return forbiddenVerdict(acct, api, resp, held)
	}
	return unavailableVerdict(fmt.Sprintf("the API answered HTTP %d, so the account could not be checked", resp.Status))
}

func probeNeedsReauth(acct vault.Account, summary string) doctorCheck {
	return doctorCheck{Status: checkFail, Code: codeNeedsReauth, Summary: summary, Fix: reconnectFix(acct)}
}

func unavailableVerdict(summary string) doctorCheck {
	return doctorCheck{Status: checkWarn, Code: probeUnavailable, Summary: summary}
}

func forbiddenVerdict(acct vault.Account, api catalog.API, resp *proxy.Response, held heldValues) doctorCheck {
	disabled, err := serviceDisabled(resp, held.secrets)
	if err != nil && !errors.Is(err, proxy.ErrCredentialEcho) {
		return unavailableVerdict("the API did not finish answering, so the account could not be checked")
	}
	if disabled && api.Service != "" {
		return doctorCheck{
			Status: checkFail, Code: probeAPIDisabled, Fix: "enable " + api.Service + " in the Cloud project", Hint: activationURL(api.Service, held.clientID),
			Summary: "the " + api.Service + " API is not enabled in the OAuth client's Cloud project",
		}
	}
	return doctorCheck{Status: checkFail, Code: probeForbidden, Fix: reconnectFix(acct), Summary: "the API refused the account (HTTP 403)"}
}

type errorDetail struct {
	Reason string `json:"reason"`
}

type errorDetails struct {
	Error struct {
		Details []errorDetail `json:"details"`
	} `json:"error"`
}

// serviceDisabled reports only whether a Google error's reason is SERVICE_DISABLED; no other value of
// the body is used or printed. The body is withheld, as if it had no such reason, when it repeats a
// token or secret in any form, raw or JSON-escaped. A non-nil err means the body could not be read.
func serviceDisabled(resp *proxy.Response, secrets []string) (bool, error) {
	if hasContentCoding(resp.Header) {
		return false, nil
	}
	guard := proxy.NewEchoGuard(append(slices.Clone(resp.Tokens), secrets...))
	inspected := io.LimitReader(resp.Body, maxProbeErrorBody+int64(guard.Lookahead()))
	data, err := io.ReadAll(io.LimitReader(guard.Reader(inspected), maxProbeErrorBody))
	if err != nil {
		return false, err
	}
	var body errorDetails
	if !trustedJSON(data, guard) || json.Unmarshal(data, &body) != nil {
		return false, nil
	}
	return slices.ContainsFunc(body.Error.Details, func(detail errorDetail) bool { return detail.Reason == reasonServiceDisabled }), nil
}

// trustedJSON reports whether data is exactly one well-formed JSON value in which no key or string, at
// any depth and including repeated members, repeats a guarded secret, and no object repeats a key
// (a decoder keeps only the last repeat, which would hide the first from every later check).
func trustedJSON(data []byte, guard *proxy.EchoGuard) bool {
	type frame struct {
		object  bool
		keyNext bool
		keys    map[string]bool
	}
	var stack []frame
	finished := false
	valueDone := func() {
		if len(stack) == 0 {
			finished = true
		} else if top := &stack[len(stack)-1]; top.object {
			top.keyNext = true
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return finished && len(stack) == 0
		}
		if err != nil || finished {
			return false
		}
		switch t := token.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, frame{object: true, keyNext: true, keys: map[string]bool{}})
			case '[':
				stack = append(stack, frame{})
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
		case string:
			if guard.Leaks(t) {
				return false
			}
			if top := len(stack) - 1; top >= 0 && stack[top].object && stack[top].keyNext {
				if stack[top].keys[t] {
					return false
				}
				stack[top].keys[t], stack[top].keyNext = true, false
				continue
			}
			valueDone()
		default:
			valueDone()
		}
	}
}

// probeFailure classifies a failure before or while sending the probe; only addition's own errors and codes leave.
func probeFailure(acct vault.Account, err error) (doctorCheck, error) {
	var refused *oauth.ProviderError
	var tokenNet *oauth.NetworkError
	var apiNet *proxy.NetworkError
	switch {
	case errors.Is(err, context.Canceled):
		return doctorCheck{}, interrupted()
	case errors.Is(err, context.DeadlineExceeded):
		return unavailableVerdict("the API did not answer in time, so the account could not be checked"), nil
	case errors.Is(err, proxy.ErrNeedsReauth), errors.Is(err, proxy.ErrNoRefreshToken), errors.Is(err, proxy.ErrNoCredential):
		return probeNeedsReauth(acct, "the account's tokens cannot be refreshed; it must be reconnected"), nil
	case errors.Is(err, proxy.ErrNoOAuthClient):
		return doctorCheck{Status: checkSkip, Summary: "not probed: no OAuth client is stored to refresh the token"}, nil
	case errors.As(err, &refused) && refused.Status < 500:
		return probeNeedsReauth(acct, "the provider refused to refresh the account's token"), nil
	case errors.As(err, &refused), errors.As(err, &tokenNet), errors.As(err, &apiNet), errors.Is(err, proxy.ErrAccountChanged):
		return unavailableVerdict("the provider could not be reached, so the account could not be checked"), nil
	}
	if reqErr, ok := errors.AsType[*proxy.RequestError](err); ok {
		return doctorCheck{}, callRequestError(reqErr)
	}
	return doctorCheck{}, vaultError(err)
}
