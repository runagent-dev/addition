package cli

import (
	"time"

	"github.com/spf13/cobra"

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
	cmd.AddCommand(&cobra.Command{
		Use:   "check [<provider>:<alias>]",
		Short: "Report each account's local token and scope status as JSON",
		Long: "Report, from the vault alone, whether each account has tokens, whether the access token\n" +
			"has expired, and which services its granted scopes cover. It does not call the provider.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runAuthCheck(cmd, d, args) },
	})
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
}

func runAuthCheck(cmd *cobra.Command, d deps, args []string) error {
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
		cred, found, err := a.credential(ctx, acct)
		if err != nil {
			return err
		}
		row := authStatus{
			Provider: acct.Provider, Alias: acct.Alias, Connected: found, Status: acct.Status,
			AccessToken: accessTokenState(cred, found, d.now()), ExpiresAt: expiryOf(cred),
			HasRefreshToken: cred.RefreshToken != "", Scopes: acct.Scopes, Services: []serviceStatus{},
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
