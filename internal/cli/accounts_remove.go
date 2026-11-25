package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/oauth"
	"github.com/runagent-dev/addition/internal/vault"
)

type revokeOutcome struct {
	Attempted bool   `json:"attempted"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

type removeResult struct {
	Provider        string        `json:"provider"`
	Alias           string        `json:"alias"`
	Removed         bool          `json:"removed"`
	Revoke          revokeOutcome `json:"revoke"`
	Revoked         bool          `json:"revoked"`
	AlsoInvalidated []string      `json:"also_invalidated"`
}

type removeFlags struct{ revoke, andSiblings bool }

func newAccountsRemoveCmd(d deps) *cobra.Command {
	var f removeFlags
	cmd := &cobra.Command{
		Use:   "remove <app>:<alias> [--revoke [--and-siblings]]",
		Short: "Delete an account and its tokens from the vault",
		Long: "Delete the account and its tokens from the vault. That is all it does by default: nothing is sent\n" +
			"to the provider, and no other account is touched.\n\n" +
			"With --revoke, first ask the provider to revoke the token; that is best effort, and the account\n" +
			"is removed locally either way. Where the provider revokes a whole grant (Google does: every token\n" +
			"the same user got through the same OAuth client, so Gmail, Drive, Calendar and the other apps of\n" +
			"that user stop working), addition refuses with revoke-affects-other-apps, naming the other accounts,\n" +
			"unless you add --and-siblings: then it revokes once and marks those accounts needs-reauth, in the\n" +
			"same step as the removal, so their next call asks you to reconnect them.\n" +
			"The result lists them in also_invalidated. To drop one app without affecting the others, omit --revoke.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runAccountsRemove(cmd, d, args[0], f) },
	}
	cmd.Flags().BoolVar(&f.revoke, "revoke", false, "revoke the token at the provider before deleting it locally")
	cmd.Flags().BoolVar(&f.andSiblings, "and-siblings", false, "with --revoke, also invalidate the other accounts the provider's revocation reaches")
	return cmd
}

func runAccountsRemove(cmd *cobra.Command, d deps, ref string, f removeFlags) error {
	ctx := cmd.Context()
	if f.andSiblings && !f.revoke {
		return usageError("--and-siblings only applies together with --revoke")
	}
	a, err := openAccountSession(ctx, d)
	if err != nil {
		return err
	}
	defer a.store.Close()
	acct, err := a.storedAccount(ctx, ref)
	if err != nil {
		return err
	}
	var siblings []vault.Account
	outcome := revokeOutcome{}
	if f.revoke {
		if siblings, err = a.grantSiblings(ctx, acct); err != nil {
			return err
		}
		if len(siblings) > 0 && !f.andSiblings {
			return a.revokeAffectsOthers(acct, siblings)
		}
		outcome = a.revoke(ctx, d, acct)
	}
	invalidated := []vault.Account{}
	if outcome.OK {
		invalidated = siblings
	}
	var marked []string
	if d.beforeRemoveCommit != nil {
		d.beforeRemoveCommit()
	}
	err = a.store.WithImmediateTx(ctx, func(tx *vault.Tx) error {
		marked = nil
		if err := tx.Audit(ctx, vault.RevokeEvent{AccountID: acct.ID}); err != nil {
			return err
		}
		if err := tx.RemoveAccount(ctx, acct.Provider, acct.Alias); err != nil {
			return err
		}
		for _, sibling := range invalidated {
			if done, err := markNeedsReauth(ctx, tx, sibling); err != nil {
				return err
			} else if done {
				marked = append(marked, accountName(sibling))
			}
		}
		return nil
	})
	if err != nil {
		return vaultError(err)
	}
	return writeJSON(cmd.OutOrStdout(), removeResult{
		Provider: acct.Provider, Alias: acct.Alias, Removed: true, Revoke: outcome, Revoked: outcome.OK, AlsoInvalidated: nonNilStrings(marked),
	})
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func accountName(acct vault.Account) string { return acct.Provider + ":" + acct.Alias }

// markNeedsReauth records that acct's grant is gone and audits it; it reports false if the account was removed meanwhile.
func markNeedsReauth(ctx context.Context, tx *vault.Tx, acct vault.Account) (bool, error) {
	current, err := tx.GetAccount(ctx, acct.Provider, acct.Alias)
	if errors.Is(err, vault.ErrNotFound) || err == nil && current.ID != acct.ID {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	current.Status = vault.AccountNeedsReauth
	if _, err := tx.UpdateAccount(ctx, current); err != nil {
		return false, err
	}
	return true, tx.Audit(ctx, vault.RevokeEvent{AccountID: current.ID})
}

// grantSiblings lists the other accounts a revocation of acct reaches: where the provider revokes a
// whole grant, those under the same identity root that belong to the same identity.
func (a *accountSession) grantSiblings(ctx context.Context, acct vault.Account) ([]vault.Account, error) {
	provider, ok := a.catalog.Provider(acct.Provider)
	if !ok || provider.OAuth2 == nil || !provider.OAuth2.RevokesGrant() {
		return nil, nil
	}
	if cred, found, err := a.credential(ctx, acct); err == nil && found && cred.Kind == vault.KindAPIKey {
		return nil, nil
	}
	all, err := a.store.ListAccounts(ctx, "")
	if err != nil {
		return nil, vaultError(err)
	}
	var siblings []vault.Account
	for _, other := range all {
		otherProvider, known := a.catalog.Provider(other.Provider)
		if other.ID != acct.ID && known && otherProvider.IdentityRoot == provider.IdentityRoot && sameIdentity(acct, other) {
			siblings = append(siblings, other)
		}
	}
	return siblings, nil
}

// sameIdentity compares stable subjects; when either account lacks one it falls back to the display identity.
func sameIdentity(a, b vault.Account) bool {
	if a.Subject != "" && b.Subject != "" {
		return a.Subject == b.Subject
	}
	return a.Identity != "" && a.Identity == b.Identity
}

func (a *accountSession) revokeAffectsOthers(acct vault.Account, siblings []vault.Account) error {
	names := make([]string, 0, len(siblings))
	for _, sibling := range siblings {
		names = append(names, accountName(sibling))
	}
	provider, _ := a.catalog.Provider(acct.Provider)
	root, _ := a.catalog.Provider(provider.IdentityRoot)
	return &Error{Code: "revoke-affects-other-apps", Exit: ExitUsage, Message: fmt.Sprintf(
		"revoking %s also revokes every %s token this user got through the same OAuth client, so it would break: %s. "+
			"Nothing was changed. Remove without --revoke to keep them, or add --and-siblings to revoke the grant and mark them needs-reauth",
		accountName(acct), root.Name, strings.Join(names, ", "))}
}

// revoke asks the provider to drop the grant, preferring the refresh token, which revokes the whole grant at Google.
func (a *accountSession) revoke(ctx context.Context, d deps, acct vault.Account) revokeOutcome {
	out := revokeOutcome{Attempted: true}
	provider, ok := a.catalog.Provider(acct.Provider)
	if !ok {
		out.Error = "the provider is no longer in the catalog"
		return out
	}
	cred, found, err := a.credential(ctx, acct)
	if err != nil || !found {
		out.Error = "the account has no stored token to revoke"
		return out
	}
	if cred.Kind == vault.KindAPIKey {
		return revokeOutcome{Error: "addition cannot revoke an API token: revoke it where you created it; nothing was sent"}
	}
	if provider.OAuth2 == nil {
		out.Error = "the provider no longer supports OAuth"
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
