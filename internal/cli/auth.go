package cli

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/oauth"
	"github.com/runagent-dev/addition/internal/proxy"
	"github.com/runagent-dev/addition/internal/vault"
)

// expiringWindow is how close to expiry an access token counts as expiring.
const expiringWindow = 5 * time.Minute

func newAuthCmd(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Check the state of connected accounts",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	var live bool
	check := &cobra.Command{
		Use:   "check [<provider>:<alias>] [--live]",
		Short: "Report each account's token and scope status as JSON",
		Long: "Report, from the vault alone, whether each account has tokens, whether the access token\n" +
			"has expired, and which services its granted scopes cover. It does not call the provider.\n\n" +
			"With --live, also refresh an expiring token and ask the provider's identity endpoint whether\n" +
			"the token works: each row gains live (true or false) and live_reason, a short code such as\n" +
			"ok, needs-reauth, token-rejected or network. A refresh is recorded like any other.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runAuthCheck(cmd, d, args, live) },
	}
	check.Flags().BoolVar(&live, "live", false, "verify each token with the provider")
	cmd.AddCommand(check)
	return cmd
}

type serviceStatus struct {
	Service       string   `json:"service"`
	Ready         bool     `json:"ready"`
	MissingScopes []string `json:"missing_scopes"`
}

type authStatus struct {
	Provider        string          `json:"provider"`
	Alias           string          `json:"alias"`
	Connected       bool            `json:"connected"`
	Status          string          `json:"status"`
	AccessToken     string          `json:"access_token"`
	ExpiresAt       *time.Time      `json:"expires_at"`
	HasRefreshToken bool            `json:"has_refresh_token"`
	Scopes          []string        `json:"scopes"`
	Services        []serviceStatus `json:"services"`
	Live            *bool           `json:"live,omitempty"`
	LiveReason      string          `json:"live_reason,omitempty"`
}

func runAuthCheck(cmd *cobra.Command, d deps, args []string, live bool) error {
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
	rows := []authStatus{}
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
		row := authStatus{
			Provider: acct.Provider, Alias: acct.Alias, Connected: found, Status: acct.Status,
			AccessToken: accessTokenState(cred, found, d.now()), ExpiresAt: expiryOf(cred),
			HasRefreshToken: cred.RefreshToken != "", Scopes: acct.Scopes, Services: []serviceStatus{},
		}
		if live {
			ok := reason == liveOK
			row.Live, row.LiveReason = &ok, reason
		}
		for _, s := range rootServices(a.catalog, acct.Provider) {
			missing := missingScopes(s.provider.OAuth2.DefaultScopes, acct.Scopes)
			row.Services = append(row.Services, serviceStatus{Service: s.name, Ready: len(missing) == 0, MissingScopes: missing})
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
// works. The result is a reason code addn chose, never provider text.
func (a *accountSession) liveCheck(ctx context.Context, d deps, acct vault.Account) string {
	provider, found := a.catalog.Provider(acct.Provider)
	if !found || provider.OAuth2 == nil || provider.OAuth2.Identity == nil {
		return "no-identity-endpoint"
	}
	tokens := &proxy.TokenManager{Store: a.store, Client: d.httpClient, Provider: provider, Account: acct, Now: d.now}
	token, err := tokens.Access(ctx)
	if err != nil {
		return liveTokenReason(err)
	}
	_, err = oauth.FetchIdentity(ctx, d.httpClient, provider, token)
	return liveIdentityReason(err)
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
