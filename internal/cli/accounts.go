package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/vault"
)

func newAccountsCmd(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "accounts",
		Short: "List, inspect, check and remove connected accounts",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return listAccounts(cmd, d) },
	}
	cmd.AddCommand(
		&cobra.Command{
			Use: "list", Short: "List connected accounts as JSON", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error { return listAccounts(cmd, d) },
		},
		&cobra.Command{
			Use: "show <app>:<alias>", Short: "Show one account as JSON", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error { return showAccount(cmd, d, args[0]) },
		},
		&cobra.Command{
			Use: "default <app>:<alias>", Short: "Use this account when a call names none for its app", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error { return setDefaultAccount(cmd, d, args[0]) },
		},
		newAccountsRemoveCmd(d),
		newAccountsCheckCmd(d),
	)
	return cmd
}

type accountRef struct{ provider, alias string }

// parseAccountRef reads <app>:<alias>, where the app is a provider id or toolkit command.
func parseAccountRef(c *catalog.Catalog, ref string) (accountRef, error) {
	name, alias, ok := strings.Cut(ref, ":")
	if !ok || name == "" || alias == "" {
		return accountRef{}, usageError(fmt.Sprintf("%q is not <app>:<alias>, e.g. gmail:work", ref))
	}
	p, found := lookupProvider(c, name)
	if !found {
		return accountRef{}, &Error{Code: "unknown-provider", Message: fmt.Sprintf("unknown provider %q (see `addn providers`)", name), Exit: ExitUsage}
	}
	return accountRef{provider: p.ID, alias: alias}, nil
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

// account loads the app account a reference names. An account stored under an identity root, as
// before apps were separate connections, is a legacy-account error.
func (a *accountSession) account(ctx context.Context, ref string) (vault.Account, error) {
	acct, err := a.storedAccount(ctx, ref)
	if err != nil {
		return vault.Account{}, err
	}
	if !a.catalog.Connectable(acct.Provider) {
		return vault.Account{}, legacyAccountError(a.catalog, acct)
	}
	return acct, nil
}

// storedAccount loads the account a reference names, legacy or not.
func (a *accountSession) storedAccount(ctx context.Context, ref string) (vault.Account, error) {
	r, err := parseAccountRef(a.catalog, ref)
	if err != nil {
		return vault.Account{}, err
	}
	acct, err := a.store.GetAccount(ctx, r.provider, r.alias)
	if errors.Is(err, vault.ErrNotFound) {
		return vault.Account{}, a.missingAccount(r)
	}
	if err != nil {
		return vault.Account{}, vaultError(err)
	}
	return acct, nil
}

func (a *accountSession) missingAccount(r accountRef) *Error {
	if p, ok := a.catalog.Provider(r.provider); ok && !a.catalog.Connectable(p.ID) {
		return notAnApp(a.catalog, p)
	}
	return notConnected(r)
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
	ScopesReady     bool       `json:"scopes_ready"`
	MissingScopes   []string   `json:"missing_scopes"`
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
	ready, missing := readiness(a.catalog, acct)
	return accountListing{
		Provider: acct.Provider, Alias: acct.Alias, Identity: acct.Identity, Scopes: acct.Scopes, Status: a.statusOf(acct),
		ScopesReady: ready, MissingScopes: missing,
		ExpiresAt: expiryOf(cred), HasRefreshToken: cred.RefreshToken != "", Default: def == acct.Alias,
		CreatedAt: acct.CreatedAt, UpdatedAt: acct.UpdatedAt,
	}, nil
}

// statusOf is the account's stored status, or legacy for one that predates per-app connections.
func (a *accountSession) statusOf(acct vault.Account) string {
	if !a.catalog.Connectable(acct.Provider) {
		return statusLegacy
	}
	return acct.Status
}

const statusLegacy = "legacy"

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
