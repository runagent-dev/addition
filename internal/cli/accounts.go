package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/oauth"
	"github.com/runagent-dev/addition/internal/vault"
)

func newAccountsCmd(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "accounts",
		Short: "List, inspect and remove connected accounts",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return listAccounts(cmd, d) },
	}
	cmd.AddCommand(
		&cobra.Command{
			Use: "list", Short: "List connected accounts as JSON", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error { return listAccounts(cmd, d) },
		},
		&cobra.Command{
			Use: "show <provider>:<alias>", Short: "Show one account as JSON", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error { return showAccount(cmd, d, args[0]) },
		},
		&cobra.Command{
			Use: "default <provider>:<alias>", Short: "Use this account when a command names none", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error { return setDefaultAccount(cmd, d, args[0]) },
		},
		newAccountsRemoveCmd(d),
	)
	return cmd
}

type accountRef struct{ provider, alias string }

// parseAccountRef reads <provider>:<alias>; a service provider such as gmail names its identity root's account.
func parseAccountRef(c *catalog.Catalog, ref string) (accountRef, error) {
	provider, alias, ok := strings.Cut(ref, ":")
	if !ok || provider == "" || alias == "" {
		return accountRef{}, usageError(fmt.Sprintf("%q is not <provider>:<alias>, e.g. google:work", ref))
	}
	root, err := c.IdentityRoot(provider)
	if err != nil {
		return accountRef{}, &Error{Code: "unknown-provider", Message: fmt.Sprintf("unknown provider %q (see `addn providers`)", provider), Exit: ExitUsage}
	}
	return accountRef{provider: root, alias: alias}, nil
}

func notConnected(ref accountRef) *Error {
	return &Error{Code: "not-connected", Exit: ExitNeedsConnect, Message: fmt.Sprintf(
		"no account %s:%s; connect it with `addn connect %s --as %s`", ref.provider, ref.alias, ref.provider, ref.alias)}
}

// accountSession is a loaded catalog and open vault for the account commands.
type accountSession struct {
	catalog *catalog.Catalog
	store   *vault.Store
}

func openAccountSession(ctx context.Context, d deps) (*accountSession, error) {
	c, err := d.loadCatalog()
	if err != nil {
		return nil, err
	}
	s, err := d.openVault(ctx)
	if err != nil {
		return nil, err
	}
	return &accountSession{catalog: c, store: s}, nil
}

func (a *accountSession) account(ctx context.Context, ref string) (vault.Account, error) {
	r, err := parseAccountRef(a.catalog, ref)
	if err != nil {
		return vault.Account{}, err
	}
	acct, err := a.store.GetAccount(ctx, r.provider, r.alias)
	if errors.Is(err, vault.ErrNotFound) {
		return vault.Account{}, notConnected(r)
	}
	if err != nil {
		return vault.Account{}, vaultError(err)
	}
	return acct, nil
}

// credential returns the account's credential; ok is false when it has none.
func (a *accountSession) credential(ctx context.Context, acct vault.Account) (vault.Credential, bool, error) {
	cred, err := a.store.GetCredential(ctx, acct.ID)
	if errors.Is(err, vault.ErrNotFound) {
		return vault.Credential{}, false, nil
	}
	if err != nil {
		return vault.Credential{}, false, vaultError(err)
	}
	return cred, true, nil
}

type accountListing struct {
	Provider        string     `json:"provider"`
	Alias           string     `json:"alias"`
	Identity        string     `json:"identity"`
	Scopes          []string   `json:"scopes"`
	Status          string     `json:"status"`
	ExpiresAt       *time.Time `json:"expires_at"`
	HasRefreshToken bool       `json:"has_refresh_token"`
	Default         bool       `json:"default"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func expiryOf(cred vault.Credential) *time.Time {
	if cred.ExpiresAt.IsZero() {
		return nil
	}
	return &cred.ExpiresAt
}

func (a *accountSession) listing(ctx context.Context, acct vault.Account) (accountListing, error) {
	cred, _, err := a.credential(ctx, acct)
	if err != nil {
		return accountListing{}, err
	}
	def, err := a.store.DefaultAccount(ctx, acct.Provider)
	if err != nil && !errors.Is(err, vault.ErrNotFound) {
		return accountListing{}, vaultError(err)
	}
	return accountListing{
		Provider: acct.Provider, Alias: acct.Alias, Identity: acct.Identity, Scopes: acct.Scopes, Status: acct.Status,
		ExpiresAt: expiryOf(cred), HasRefreshToken: cred.RefreshToken != "", Default: def == acct.Alias,
		CreatedAt: acct.CreatedAt, UpdatedAt: acct.UpdatedAt,
	}, nil
}

func listAccounts(cmd *cobra.Command, d deps) error {
	ctx := cmd.Context()
	a, err := openAccountSession(ctx, d)
	if err != nil {
		return err
	}
	defer a.store.Close()
	accts, err := a.store.ListAccounts(ctx, "")
	if err != nil {
		return vaultError(err)
	}
	rows := []accountListing{}
	for _, acct := range accts {
		row, err := a.listing(ctx, acct)
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}
	return writeJSON(cmd.OutOrStdout(), rows)
}

func showAccount(cmd *cobra.Command, d deps, ref string) error {
	ctx := cmd.Context()
	a, err := openAccountSession(ctx, d)
	if err != nil {
		return err
	}
	defer a.store.Close()
	acct, err := a.account(ctx, ref)
	if err != nil {
		return err
	}
	row, err := a.listing(ctx, acct)
	if err != nil {
		return err
	}
	return writeJSON(cmd.OutOrStdout(), row)
}

func setDefaultAccount(cmd *cobra.Command, d deps, ref string) error {
	ctx := cmd.Context()
	a, err := openAccountSession(ctx, d)
	if err != nil {
		return err
	}
	defer a.store.Close()
	acct, err := a.account(ctx, ref)
	if err != nil {
		return err
	}
	if err := a.store.SetDefaultAccount(ctx, acct.Provider, acct.Alias); err != nil {
		return vaultError(err)
	}
	return writeJSON(cmd.OutOrStdout(), map[string]string{"provider": acct.Provider, "default": acct.Alias})
}

type revokeOutcome struct {
	Attempted bool   `json:"attempted"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

func newAccountsRemoveCmd(d deps) *cobra.Command {
	var revoke bool
	cmd := &cobra.Command{
		Use:   "remove <provider>:<alias> [--revoke]",
		Short: "Delete an account and its tokens from the vault",
		Long: "Delete the account and its tokens. With --revoke, first ask the provider to revoke the\n" +
			"grant; that is best effort, and the account is removed locally either way.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			a, err := openAccountSession(ctx, d)
			if err != nil {
				return err
			}
			defer a.store.Close()
			acct, err := a.account(ctx, args[0])
			if err != nil {
				return err
			}
			outcome := revokeOutcome{}
			if revoke {
				outcome = a.revoke(ctx, d, acct)
			}
			err = a.store.WithImmediateTx(ctx, func(tx *vault.Tx) error {
				if err := tx.Audit(ctx, vault.RevokeEvent{AccountID: acct.ID}); err != nil {
					return err
				}
				return tx.RemoveAccount(ctx, acct.Provider, acct.Alias)
			})
			if err != nil {
				return vaultError(err)
			}
			return writeJSON(cmd.OutOrStdout(), map[string]any{
				"provider": acct.Provider, "alias": acct.Alias, "removed": true, "revoke": outcome,
			})
		},
	}
	cmd.Flags().BoolVar(&revoke, "revoke", false, "revoke the grant at the provider before deleting it locally")
	return cmd
}

// revoke asks the provider to drop the grant, preferring the refresh token, which revokes the whole grant at Google.
func (a *accountSession) revoke(ctx context.Context, d deps, acct vault.Account) revokeOutcome {
	out := revokeOutcome{Attempted: true}
	provider, ok := a.catalog.Provider(acct.Provider)
	if !ok || provider.OAuth2 == nil {
		out.Error = "the provider is no longer in the catalog"
		return out
	}
	cred, found, err := a.credential(ctx, acct)
	if err != nil || !found {
		out.Error = "the account has no stored token to revoke"
		return out
	}
	token := cred.RefreshToken
	if token == "" {
		token = cred.AccessToken
	}
	if err := oauth.Revoke(ctx, d.httpClient, provider, token); err != nil {
		out.Error = err.Error()
		return out
	}
	out.OK = true
	return out
}
