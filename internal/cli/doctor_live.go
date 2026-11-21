package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
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

// probeTimeout bounds one probe, refresh and error body included.
var probeTimeout = defaultProbeTimeout

// activationHosts are the only hosts a provider's activation link may name before its project id is used.
var activationHosts = []string{"console.developers.google.com", "console.cloud.google.com"}

// projectPattern matches a Cloud project id or number, so nothing else a provider says is repeated.
var projectPattern = regexp.MustCompile(`^(?:[a-z][a-z0-9-]{4,28}[a-z0-9]|[0-9]{4,20})$`)

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
	tokens := &proxy.TokenManager{Store: a.store, Client: d.httpClient, Provider: provider, Account: acct, Now: d.now}
	token, err := tokens.Access(ctx)
	if err != nil {
		return probeFailure(acct, err)
	}
	engine := &proxy.Engine{Client: d.httpClient, API: provider.API, AllowCleartext: d.loopbackProviders, Renewer: tokens, Now: d.now, Sleep: d.sleep}
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
	return probeVerdict(acct, provider.API, resp, a.storedSecrets(ctx, acct, provider)), nil
}

// storedSecrets are the refresh token and OAuth client secret a response must never repeat.
func (a *accountSession) storedSecrets(ctx context.Context, acct vault.Account, provider *catalog.Provider) []string {
	var secrets []string
	if cred, found, err := a.credential(ctx, acct); err == nil && found && cred.RefreshToken != "" {
		secrets = append(secrets, cred.RefreshToken)
	}
	if client, err := a.store.GetOAuthClient(ctx, provider.IdentityRoot); err == nil && client.ClientSecret != "" {
		secrets = append(secrets, client.ClientSecret)
	}
	return secrets
}

func probeVerdict(acct vault.Account, api catalog.API, resp *proxy.Response, secrets []string) doctorCheck {
	switch {
	case resp.Status >= 200 && resp.Status <= 299:
		return doctorCheck{Status: checkOK, Code: checkOK, Summary: fmt.Sprintf("the API accepted the account's token (HTTP %d)", resp.Status)}
	case resp.Status == http.StatusUnauthorized:
		return probeNeedsReauth(acct, "the API rejected the account's token (HTTP 401)")
	case resp.Status == http.StatusForbidden:
		return forbiddenVerdict(acct, api, resp, secrets)
	}
	return unavailableVerdict(fmt.Sprintf("the API answered HTTP %d, so the account could not be checked", resp.Status))
}

func probeNeedsReauth(acct vault.Account, summary string) doctorCheck {
	return doctorCheck{Status: checkFail, Code: codeNeedsReauth, Summary: summary, Fix: reconnectFix(acct)}
}

func unavailableVerdict(summary string) doctorCheck {
	return doctorCheck{Status: checkWarn, Code: probeUnavailable, Summary: summary}
}

func forbiddenVerdict(acct vault.Account, api catalog.API, resp *proxy.Response, secrets []string) doctorCheck {
	disabled, project, err := readServiceDisabled(resp, secrets)
	if errors.Is(err, context.DeadlineExceeded) {
		return unavailableVerdict("the API did not finish answering in time, so the account could not be checked")
	}
	if disabled && api.Service != "" {
		return doctorCheck{
			Status: checkFail, Code: probeAPIDisabled, Fix: "enable " + api.Service + " in the Cloud project", Hint: activationURL(api.Service, project),
			Summary: "the " + api.Service + " API is not enabled in the OAuth client's Cloud project",
		}
	}
	return doctorCheck{Status: checkFail, Code: probeForbidden, Fix: reconnectFix(acct), Summary: "the API refused the account (HTTP 403)"}
}

type errorDetails struct {
	Error struct {
		Details []struct {
			Reason   string `json:"reason"`
			Metadata struct {
				ActivationURL string `json:"activationUrl"`
			} `json:"metadata"`
		} `json:"details"`
	} `json:"error"`
}

// readServiceDisabled takes from a Google error only whether its reason is SERVICE_DISABLED and a
// project id of the expected shape from its activation link; nothing else is repeated. The body is
// withheld, as if it had no such reason, when it repeats a token or secret in any form, raw or JSON-escaped.
// A non-nil err means the body could not be read; it is context.DeadlineExceeded when the probe ran out of time.
func readServiceDisabled(resp *proxy.Response, secrets []string) (disabled bool, project string, err error) {
	if hasContentCoding(resp.Header) {
		return false, "", nil
	}
	guard := proxy.NewEchoGuard(append(slices.Clone(resp.Tokens), secrets...))
	inspected := io.LimitReader(resp.Body, maxProbeErrorBody+int64(guard.Lookahead()))
	data, err := io.ReadAll(io.LimitReader(guard.Reader(inspected), maxProbeErrorBody))
	if err != nil {
		return false, "", err
	}
	var generic any
	var body errorDetails
	if json.Unmarshal(data, &generic) != nil || json.Unmarshal(data, &body) != nil || decodedLeaks(generic, guard) {
		return false, "", nil
	}
	for _, detail := range body.Error.Details {
		if detail.Reason == reasonServiceDisabled {
			return true, projectOf(detail.Metadata.ActivationURL), nil
		}
	}
	return false, "", nil
}

// decodedLeaks reports whether any decoded key or string of a JSON value repeats a guarded secret.
func decodedLeaks(v any, guard *proxy.EchoGuard) bool {
	switch v := v.(type) {
	case string:
		return guard.Leaks(v)
	case []any:
		return slices.ContainsFunc(v, func(item any) bool { return decodedLeaks(item, guard) })
	case map[string]any:
		for key, item := range v {
			if guard.Leaks(key) || decodedLeaks(item, guard) {
				return true
			}
		}
	}
	return false
}

// projectOf returns the project of a Google console link, or "" when the link or the id is not what Google sends.
func projectOf(rawLink string) string {
	link, err := url.Parse(rawLink)
	if err != nil || link.Scheme != "https" || link.User != nil || link.Port() != "" || !slices.Contains(activationHosts, link.Hostname()) {
		return ""
	}
	if project := link.Query().Get("project"); projectPattern.MatchString(project) {
		return project
	}
	return ""
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
