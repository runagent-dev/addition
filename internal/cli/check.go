package cli

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/oauth"
	"github.com/runagent-dev/addition/internal/proxy"
	"github.com/runagent-dev/addition/internal/vault"
)

// expiringWindow is how close to expiry an access token counts as expiring.
const expiringWindow = 5 * time.Minute

// newAuthCmd is the deprecated `addition auth check`, hidden from help and running `addition accounts check`.
func newAuthCmd(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "auth",
		Short:  "Deprecated: use `addition accounts check`",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE:   func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newAccountsCheckCmd(d))
	return cmd
}

func newAccountsCheckCmd(d deps) *cobra.Command {
	var live bool
	check := &cobra.Command{
		Use:   "check [<app>:<alias>] [--live]",
		Short: "Report each account's token and scope status as JSON",
		Long: "Report, from the vault alone, whether each account has tokens, whether the access token\n" +
			"has expired, and whether its granted scopes cover its app's (scopes_ready, missing_scopes).\n" +
			"It does not call the provider.\n\n" +
			"With --live, also refresh an expiring token and ask the provider's identity endpoint whether\n" +
			"the token works: each row gains live (true or false) and live_reason, a short code such as\n" +
			"ok, needs-reauth, token-rejected or network. A refresh is recorded like any other.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runAccountsCheck(cmd, d, args, live) },
	}
	check.Flags().BoolVar(&live, "live", false, "verify each token with the provider")
	return check
}

type accountCheck struct {
	Provider        string     `json:"provider"`
	Alias           string     `json:"alias"`
	Connected       bool       `json:"connected"`
	Method          string     `json:"method"`
	Status          string     `json:"status"`
	AccessToken     string     `json:"access_token"`
	ExpiresAt       *time.Time `json:"expires_at"`
	HasRefreshToken bool       `json:"has_refresh_token"`
	Scopes          []string   `json:"scopes"`
	ScopesReady     bool       `json:"scopes_ready"`
	MissingScopes   []string   `json:"missing_scopes"`
	Live            *bool      `json:"live,omitempty"`
	LiveReason      string     `json:"live_reason,omitempty"`
}

func runAccountsCheck(cmd *cobra.Command, d deps, args []string, live bool) error {
	ctx := cmd.Context()
	a, err := openAccountSession(ctx, d)
	if err != nil {
		return err
	}
	defer a.store.Close()
	var accts []vault.Account
	if len(args) == 1 {
		acct, err := a.account(ctx, args[0])
		if err != nil {
			return err
		}
		accts = []vault.Account{acct}
	} else if accts, err = a.store.ListAccounts(ctx, ""); err != nil {
		return vaultError(err)
	}
	rows := []accountCheck{}
	for _, acct := range accts {
		reason := ""
		if live {
			reason = a.liveCheck(ctx, d, acct)
			if acct, err = a.store.GetAccount(ctx, acct.Provider, acct.Alias); err != nil {
				return vaultError(err)
			}
		}
		cred, found, err := a.credential(ctx, acct)
		if err != nil {
			return err
		}
		ready, missing := readiness(a.catalog, acct, credentialKind(cred, found))
		row := accountCheck{
			Provider: acct.Provider, Alias: acct.Alias, Connected: found, Method: credentialKind(cred, found), Status: a.statusOf(acct),
			AccessToken: accessTokenState(cred, found, d.now()), ExpiresAt: expiryOf(cred),
			HasRefreshToken: cred.RefreshToken != "", Scopes: acct.Scopes, ScopesReady: ready, MissingScopes: missing,
		}
		if live {
			ok := reason == liveOK
			row.Live, row.LiveReason = &ok, reason
		}
		rows = append(rows, row)
	}
	return writeJSON(cmd.OutOrStdout(), rows)
}

func accessTokenState(cred vault.Credential, found bool, now time.Time) string {
	switch {
	case !found:
		return "missing"
	case cred.ExpiresAt.IsZero():
		return "no-expiry"
	case !now.Before(cred.ExpiresAt):
		return "expired"
	case cred.ExpiresAt.Sub(now) <= expiringWindow:
		return "expiring"
	}
	return "valid"
}

const liveOK = "ok"

// liveCheck refreshes the account's token if needed and asks the identity endpoint whether it
// works. The result is a reason code addition chose, never provider text.
func (a *accountSession) liveCheck(ctx context.Context, d deps, acct vault.Account) string {
	provider, found := a.catalog.Provider(acct.Provider)
	if found && !a.catalog.Connectable(provider.ID) {
		return "legacy-account"
	}
	if found {
		if cred, has, err := a.credential(ctx, acct); err == nil && has && cred.Kind == vault.KindAPIKey {
			return a.liveCheckToken(ctx, d, provider, acct)
		}
	}
	if !found || provider.OAuth2 == nil || provider.OAuth2.Identity == nil {
		return "no-identity-endpoint"
	}
	if !identityURLAllowed(provider, d.loopbackProviders) {
		return "identity-not-allowed"
	}
	tokens := &proxy.TokenManager{Store: a.store, Client: d.httpClient, Provider: provider, Account: acct, Now: d.now}
	token, err := tokens.Access(ctx)
	if err != nil {
		return liveTokenReason(err)
	}
	_, err = oauth.FetchIdentity(ctx, d.httpClient, provider, token)
	return liveIdentityReason(err)
}

// identityURLAllowed checks the identity endpoint the token is about to be sent to against the provider's allowed hosts.
func identityURLAllowed(provider *catalog.Provider, allowCleartext bool) bool {
	target, err := url.Parse(provider.OAuth2.Identity.URL)
	return err == nil && proxy.CheckDestination(provider.API, target, allowCleartext) == nil
}

func liveTokenReason(err error) string {
	var refused *oauth.ProviderError
	var unreachable *oauth.NetworkError
	switch {
	case errors.Is(err, proxy.ErrNeedsReauth):
		return "needs-reauth"
	case errors.Is(err, proxy.ErrNoRefreshToken):
		return "no-refresh-token"
	case errors.Is(err, proxy.ErrNoCredential):
		return "no-credential"
	case errors.Is(err, proxy.ErrNoOAuthClient):
		return "no-oauth-client"
	case errors.As(err, &refused):
		return "refresh-rejected"
	case errors.As(err, &unreachable):
		return "network"
	}
	return "internal"
}

func liveIdentityReason(err error) string {
	var refused *oauth.ProviderError
	var unreachable *oauth.NetworkError
	switch {
	case err == nil:
		return liveOK
	case errors.As(err, &refused) && (refused.Status == http.StatusUnauthorized || refused.Status == http.StatusForbidden):
		return "token-rejected"
	case errors.As(err, &refused):
		return "identity-rejected"
	case errors.As(err, &unreachable):
		return "network"
	}
	return "internal"
}

// liveCheckToken asks the app's identity endpoint, or its probe path, whether an API token still works.
// A token is never refreshed, and a rejection is reported without changing the account.
func (a *accountSession) liveCheckToken(ctx context.Context, d deps, provider *catalog.Provider, acct vault.Account) string {
	if provider.APIKey == nil {
		return "no-identity-endpoint"
	}
	tokens := &proxy.TokenManager{Store: a.store, Client: d.httpClient, Provider: provider, Account: acct, Now: d.now}
	token, err := tokens.Access(ctx)
	if err != nil {
		return liveTokenReason(err)
	}
	resp, err := sendVerification(ctx, d, provider, acct.Alias, token)
	if err != nil {
		return liveVerificationReason(err)
	}
	_ = resp.Body.Close()
	return liveStatusReason(resp.Status)
}

func liveStatusReason(status int) string {
	switch {
	case status >= 200 && status <= 299:
		return liveOK
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "token-rejected"
	}
	return "identity-rejected"
}

func liveVerificationReason(err error) string {
	if typed, ok := errors.AsType[*Error](err); ok {
		switch typed.Code {
		case "invalid-token":
			return "token-rejected"
		case "network":
			return "network"
		}
	}
	return "internal"
}
