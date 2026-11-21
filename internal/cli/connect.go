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

const pendingAuthTTL = 10 * time.Minute

type connectFlags struct {
	alias     string
	noBrowser bool
	complete  string
}

func newConnectCmd(d deps) *cobra.Command {
	var f connectFlags
	cmd := &cobra.Command{
		Use:   "connect <app> --as <alias> [--no-browser]",
		Short: "Connect one app's account through its OAuth consent screen",
		Long: "Connect an account of one app, such as gmail, google-drive or google-chat (see `addn providers`).\n" +
			"Each app is its own connection: it asks only for its own scopes and can be removed on its own.\n" +
			"The same alias may be used in several apps. The app is a provider id or toolkit command\n" +
			"(drive or google-drive).\n\n" +
			"By default addn opens the consent screen in your browser and waits up to 5 minutes for\n" +
			"the redirect to a listener on 127.0.0.1.\n\n" +
			"Headless or over SSH, use two steps:\n" +
			"  addn connect gmail --as work --no-browser\n" +
			"      prints JSON with authorize_url; open it on any device and approve. The browser then\n" +
			"      lands on http://127.0.0.1:<port>/callback?code=…&state=…, a page that fails to load;\n" +
			"      copy the full URL from the address bar.\n" +
			"  addn connect --complete '<that URL>'\n" +
			"      finishes the connection. Each authorization works once, for 10 minutes.\n\n" +
			"Reconnecting an alias asks for the union of the scopes already granted and the app's own,\n" +
			"and stores what the provider actually granted.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.complete != "" {
				if len(args) > 0 || f.alias != "" || f.noBrowser {
					return usageError("--complete takes only the redirect URL")
				}
				return completeTwoStep(cmd, d, f.complete)
			}
			if len(args) != 1 {
				return usageError("connect needs an app, e.g. `addn connect gmail --as work`")
			}
			if f.alias == "" {
				return usageError("connect needs --as <alias>")
			}
			return startConnect(cmd, d, args[0], f)
		},
	}
	cmd.Flags().StringVar(&f.alias, "as", "", "alias for the account, e.g. work")
	cmd.Flags().BoolVar(&f.noBrowser, "no-browser", false, "print the authorize URL and finish later with --complete")
	cmd.Flags().StringVar(&f.complete, "complete", "", "finish a --no-browser connect with the URL the browser landed on")
	return cmd
}

// connectTarget is everything both flows resolve before talking to the provider.
// accountScopes is what the alias held when the authorization started.
type connectTarget struct {
	provider      *catalog.Provider
	client        oauth.Client
	alias         string
	scopes        []string
	accountScopes []string
}

func startConnect(cmd *cobra.Command, d deps, providerID string, f connectFlags) error {
	ctx := cmd.Context()
	c, err := d.loadCatalog()
	if err != nil {
		return err
	}
	provider, err := connectableApp(c, providerID)
	if err != nil {
		return err
	}
	if err := vault.ValidateAccountName(provider.ID, f.alias); err != nil {
		return vaultError(err)
	}
	s, err := d.openVault(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	target, err := loadTarget(ctx, s, provider, f.alias)
	if err != nil {
		return err
	}
	target.scopes = unionScopes(target.accountScopes, provider.OAuth2.DefaultScopes)
	if f.noBrowser {
		return startTwoStep(cmd, d, s, target)
	}
	return connectViaLoopback(cmd, d, s, target)
}

// connectableApp resolves name to an app that signs in with OAuth.
func connectableApp(c *catalog.Catalog, name string) (*catalog.Provider, error) {
	p, err := appProvider(c, name)
	if err != nil {
		return nil, err
	}
	if p.AuthType != "oauth2" || p.OAuth2 == nil {
		return nil, &Error{Code: "not-oauth", Message: fmt.Sprintf("%s does not use OAuth", p.ID), Exit: ExitUsage}
	}
	return p, nil
}

// loadTarget loads the identity root's OAuth client and the scopes the app's alias already holds.
func loadTarget(ctx context.Context, s *vault.Store, provider *catalog.Provider, alias string) (connectTarget, error) {
	client, err := s.GetOAuthClient(ctx, provider.IdentityRoot)
	if errors.Is(err, vault.ErrNotFound) {
		return connectTarget{}, &Error{Code: "no-oauth-client", Exit: ExitUsage, Message: fmt.Sprintf(
			"no OAuth client for %s; create a \"Desktop app\" client and run `addn client add %s --file client_secret.json`",
			provider.ID, provider.IdentityRoot)}
	}
	if err != nil {
		return connectTarget{}, vaultError(err)
	}
	t := connectTarget{provider: provider, client: oauth.Client{ID: client.ClientID, Secret: client.ClientSecret}, alias: alias}
	existing, err := s.GetAccount(ctx, provider.ID, alias)
	if err == nil {
		t.accountScopes = existing.Scopes
	} else if !errors.Is(err, vault.ErrNotFound) {
		return connectTarget{}, vaultError(err)
	}
	return t, nil
}

func connectViaLoopback(cmd *cobra.Command, d deps, s *vault.Store, t connectTarget) error {
	auth, err := oauth.NewAuthorization(t.provider.OAuth2, "", t.scopes)
	if err != nil {
		return err
	}
	lb, err := oauth.ListenLoopback(0, auth.State)
	if err != nil {
		return &Error{Code: "loopback", Message: "could not listen on 127.0.0.1", Exit: ExitInternal}
	}
	defer lb.Close()
	auth.RedirectURI = lb.RedirectURI()
	cb, err := awaitCallback(cmd, d, lb, auth.URL(t.provider.OAuth2, t.client))
	if err != nil {
		return err
	}
	_ = lb.Close()
	code, err := cb.Verify(auth.State)
	if err != nil {
		return oauthError(err)
	}
	g, err := redeem(cmd.Context(), d, t, auth, code)
	if err != nil {
		return oauthError(err)
	}
	result, err := commitConnect(cmd.Context(), d, s, t, auth, g, nil)
	if err != nil {
		return commitError(err)
	}
	return reportConnect(cmd, t, auth, result)
}

// awaitCallback opens the browser and waits for the loopback's verified callback. If the
// launcher fails, it prints a notice with the URL to stderr, one JSON object per line.
func awaitCallback(cmd *cobra.Command, d deps, lb *oauth.Loopback, authorizeURL string) (oauth.Callback, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), d.loopbackTimeout)
	defer cancel()
	launcher := d.openBrowser(authorizeURL)
	for {
		select {
		case cb := <-lb.Received():
			return cb, nil
		case err := <-launcher:
			launcher = nil
			if err != nil {
				_ = writeJSON(cmd.ErrOrStderr(), map[string]string{"notice": "open-browser-failed", "authorize_url": authorizeURL})
			}
		case <-ctx.Done():
			if rejected := lb.LastRejection(); rejected != nil {
				return oauth.Callback{}, oauthError(rejected)
			}
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return oauth.Callback{}, ctx.Err()
			}
			return oauth.Callback{}, &Error{Code: "authorization-timeout", Exit: ExitUsage, Message: fmt.Sprintf(
				"no authorization arrived within %s; run connect again, or use --no-browser on a headless machine", d.loopbackTimeout)}
		}
	}
}

func startTwoStep(cmd *cobra.Command, d deps, s *vault.Store, t connectTarget) error {
	port, err := oauth.FreeLoopbackPort()
	if err != nil {
		return &Error{Code: "loopback", Message: "could not pick a port on 127.0.0.1", Exit: ExitInternal}
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	auth, err := oauth.NewAuthorization(t.provider.OAuth2, redirectURI, t.scopes)
	if err != nil {
		return err
	}
	expires := d.now().Add(pendingAuthTTL).UTC().Truncate(time.Second)
	err = s.PutPendingAuth(cmd.Context(), vault.PendingAuth{
		State: auth.State, Provider: t.provider.ID, Alias: t.alias, Scopes: t.scopes, AccountScopes: t.accountScopes,
		RedirectURI: redirectURI, Verifier: auth.Verifier, ExpiresAt: expires,
	})
	if err != nil {
		return vaultError(err)
	}
	return writeJSON(cmd.OutOrStdout(), map[string]string{
		"authorize_url": auth.URL(t.provider.OAuth2, t.client),
		"expires_at":    expires.Format(time.RFC3339),
		"next":          "addn connect --complete '<redirect-url>'",
	})
}

// completeTwoStep checks the pasted redirect against its pending authorization before claiming
// it, so a bad paste leaves it usable; the claim then keeps a second completion from exchanging.
func completeTwoStep(cmd *cobra.Command, d deps, pasted string) error {
	ctx := cmd.Context()
	cb, err := oauth.ParseCallback(pasted)
	if err != nil {
		return oauthError(err)
	}
	c, err := d.loadCatalog()
	if err != nil {
		return err
	}
	s, err := d.openVault(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	pending, err := s.GetPendingAuth(ctx, cb.State)
	if err != nil {
		return pendingError(err)
	}
	if cb.RedirectURI != pending.RedirectURI {
		return &Error{Code: "redirect-mismatch", Exit: ExitUsage,
			Message: fmt.Sprintf("the URL is not the redirect this authorization expects (%s)", pending.RedirectURI)}
	}
	cleanup := context.WithoutCancel(ctx)
	code, err := cb.Verify(pending.State)
	if errors.As(err, new(*oauth.AuthorizationError)) {
		if err := s.DeleteUnclaimedPendingAuth(cleanup, pending.State); err != nil {
			return pendingError(err)
		}
	}
	if err != nil {
		return oauthError(err)
	}
	provider, err := connectableApp(c, pending.Provider)
	if err != nil {
		return err
	}
	t, err := loadTarget(ctx, s, provider, pending.Alias)
	if err != nil {
		return err
	}
	t.scopes, t.accountScopes = pending.Scopes, pending.AccountScopes
	if _, err := s.ClaimPendingAuth(ctx, pending.State); err != nil {
		return pendingError(err)
	}

	auth := oauth.Authorization{State: pending.State, Verifier: pending.Verifier, RedirectURI: pending.RedirectURI, Scopes: pending.Scopes}
	again := fmt.Sprintf("; this authorization can no longer be used, so run `addn connect %s --as %s --no-browser` again",
		pending.Provider, pending.Alias)
	g, err := redeem(ctx, d, t, auth, code)
	if err != nil {
		var netErr *oauth.NetworkError
		if errors.As(err, &netErr) && netErr.Op == oauth.OpExchange && !netErr.RequestSent {
			_ = s.ReleasePendingAuth(cleanup, pending.State)
			return oauthError(err)
		}
		_ = s.DeletePendingAuth(cleanup, pending.State)
		return withHint(oauthError(err), again)
	}
	result, err := commitConnect(ctx, d, s, t, auth, g, func(tx *vault.Tx) error {
		return tx.DeletePendingAuth(ctx, pending.State)
	})
	if err != nil {
		_ = s.DeletePendingAuth(cleanup, pending.State)
		if typed, ok := errors.AsType[*Error](err); ok {
			return typed
		}
		return withHint(vaultError(err), again)
	}
	return reportConnect(cmd, t, auth, result)
}

func pendingError(err error) error {
	switch {
	case errors.Is(err, vault.ErrNotFound):
		return &Error{Code: "unknown-state", Exit: ExitUsage,
			Message: "no pending authorization matches this URL's state; it was already used or never issued. Run `addn connect <provider> --as <alias> --no-browser` again"}
	case errors.Is(err, vault.ErrPendingExpired):
		return &Error{Code: "authorization-expired", Exit: ExitUsage,
			Message: "this authorization expired; run `addn connect <provider> --as <alias> --no-browser` again"}
	case errors.Is(err, vault.ErrPendingClaimed):
		return &Error{Code: "authorization-in-progress", Exit: ExitUsage,
			Message: "another `addn connect --complete` is finishing this authorization; if it was interrupted, run `addn connect <provider> --as <alias> --no-browser` again"}
	}
	return vaultError(err)
}

func withHint(e *Error, hint string) *Error {
	e.Message += hint
	return e
}

type connectResult struct {
	Provider string   `json:"provider"`
	Alias    string   `json:"alias"`
	Identity string   `json:"identity"`
	Scopes   []string `json:"scopes"`
	Created  bool     `json:"created"`
}

// grant is what an authorization code bought: the tokens and who they belong to.
type grant struct {
	tokens   oauth.Tokens
	identity oauth.Identity
}

func redeem(ctx context.Context, d deps, t connectTarget, auth oauth.Authorization, code string) (grant, error) {
	tokens, err := oauth.Exchange(ctx, d.httpClient, t.provider, t.client, auth, code)
	if err != nil {
		return grant{}, err
	}
	identity, err := oauth.FetchIdentity(ctx, d.httpClient, t.provider, tokens.AccessToken)
	if err != nil {
		return grant{}, err
	}
	return grant{tokens: tokens, identity: identity}, nil
}

// commitConnect stores the account, its credential and an audit row in one transaction, with
// settle's work if given. A typed *Error is a refusal; any other error is the vault failing.
func commitConnect(ctx context.Context, d deps, s *vault.Store, t connectTarget, auth oauth.Authorization, g grant, settle func(*vault.Tx) error) (connectResult, error) {
	granted := g.tokens.Scopes
	if granted == nil {
		granted = auth.Scopes
	}
	result := connectResult{Provider: t.provider.ID, Alias: t.alias, Identity: g.identity.Display, Scopes: granted}
	err := s.WithImmediateTx(ctx, func(tx *vault.Tx) error {
		saved, err := saveAccount(ctx, tx, t, auth, result, g.identity.Subject)
		if err != nil {
			return err
		}
		acct := saved.account
		result.Created = saved.created
		cred := vault.Credential{AccessToken: g.tokens.AccessToken, RefreshToken: g.tokens.RefreshToken, TokenType: g.tokens.TokenType}
		if cred.RefreshToken == "" && saved.sameSubject {
			if old, err := tx.GetCredential(ctx, acct.ID); err == nil {
				cred.RefreshToken = old.RefreshToken
			}
		}
		if g.tokens.ExpiresIn > 0 {
			cred.ExpiresAt = d.now().Add(g.tokens.ExpiresIn)
		}
		if err := tx.PutCredential(ctx, acct.ID, cred); err != nil {
			return err
		}
		if err := tx.Audit(ctx, vault.ConnectEvent{AccountID: acct.ID}); err != nil {
			return err
		}
		if settle != nil {
			return settle(tx)
		}
		return nil
	})
	return result, err
}

func commitError(err error) error {
	if typed, ok := errors.AsType[*Error](err); ok {
		return typed
	}
	return vaultError(err)
}

// reportConnect prints the stored account, then flags any requested scope the provider withheld.
func reportConnect(cmd *cobra.Command, t connectTarget, auth oauth.Authorization, result connectResult) error {
	if err := writeJSON(cmd.OutOrStdout(), result); err != nil {
		return err
	}
	if missing := missingScopes(auth.Scopes, result.Scopes); len(missing) > 0 {
		return &Error{Code: "scopes-not-granted", Exit: ExitNeedsConnect, Message: fmt.Sprintf(
			"the account was saved, but %s did not grant: %s; connect again and allow them on the consent screen",
			t.provider.ID, strings.Join(missing, " "))}
	}
	return nil
}

// savedAccount is the stored alias. sameSubject is true when it was already bound to the
// consenting subject, the only case where its old credential is the same account's.
type savedAccount struct {
	account     vault.Account
	created     bool
	sameSubject bool
}

// saveAccount creates the alias, or updates it when the same subject re-consents with a fresh
// view of its scopes.
func saveAccount(ctx context.Context, tx *vault.Tx, t connectTarget, auth oauth.Authorization, r connectResult, subject string) (savedAccount, error) {
	existing, err := tx.GetAccount(ctx, r.Provider, r.Alias)
	if errors.Is(err, vault.ErrNotFound) {
		acct, err := tx.CreateAccount(ctx, vault.Account{Provider: r.Provider, Alias: r.Alias, Identity: r.Identity, Subject: subject, Scopes: r.Scopes})
		return savedAccount{account: acct, created: true}, err
	}
	if err != nil {
		return savedAccount{}, err
	}
	if !matchesAccount(existing, subject, r.Identity) {
		return savedAccount{}, &Error{Code: "identity-mismatch", Exit: ExitUsage, Message: fmt.Sprintf(
			"%s:%s is %s, but the consent was given by another account (%s); nothing was changed. Connect that account under another alias",
			r.Provider, r.Alias, existing.Identity, r.Identity)}
	}
	if added := missingScopes(existing.Scopes, t.accountScopes); len(missingScopes(added, auth.Scopes)) > 0 {
		return savedAccount{}, &Error{Code: "stale-authorization", Exit: ExitUsage, Message: fmt.Sprintf(
			"%s:%s gained %s after this authorization started, and it did not ask for them; nothing was changed. Run `addn connect %s --as %s` again to authorize every scope",
			r.Provider, r.Alias, strings.Join(missingScopes(added, auth.Scopes), " "), r.Provider, r.Alias)}
	}
	sameSubject := existing.Subject != "" && existing.Subject == subject
	existing.Identity, existing.Subject, existing.Scopes, existing.Status = r.Identity, subject, r.Scopes, vault.AccountActive
	acct, err := tx.UpdateAccount(ctx, existing)
	return savedAccount{account: acct, sameSubject: sameSubject}, err
}

// matchesAccount matches on the provider's stable subject; an account stored without one falls
// back to its display identity, and is then bound to the subject without keeping its credential.
func matchesAccount(existing vault.Account, subject, display string) bool {
	if existing.Subject != "" {
		return existing.Subject == subject
	}
	return existing.Identity == "" || existing.Identity == display
}

// oauthError maps engine failures to the exit codes: 2 for a bad paste or state, 4 for a provider
// refusal or an unusable 2xx, 5 for the network or a provider 5xx. Every message is addn's own.
func oauthError(err error) *Error {
	var authErr *oauth.AuthorizationError
	var provErr *oauth.ProviderError
	var netErr *oauth.NetworkError
	switch {
	case errors.Is(err, oauth.ErrBadRedirect):
		return &Error{Code: "bad-redirect", Message: err.Error(), Exit: ExitUsage}
	case errors.Is(err, oauth.ErrStateMismatch):
		return &Error{Code: "state-mismatch", Message: err.Error(), Exit: ExitUsage}
	case errors.As(err, &authErr):
		code := "authorization-error"
		if authErr.Code == "access_denied" {
			code = "access-denied"
		}
		return &Error{Code: code, Message: err.Error(), Exit: ExitUpstreamClient}
	case errors.As(err, &provErr):
		exit := ExitUpstreamClient
		if provErr.Status >= 500 {
			exit = ExitUpstreamServer
		}
		return &Error{Code: providerErrorCodes[provErr.Op], Message: err.Error(), Exit: exit}
	case errors.As(err, &netErr):
		return &Error{Code: "network", Message: err.Error(), Exit: ExitUpstreamServer}
	}
	return &Error{Code: "internal", Message: "the OAuth flow failed inside addn", Exit: ExitInternal}
}

var providerErrorCodes = map[string]string{
	oauth.OpExchange: "token-exchange-rejected",
	oauth.OpIdentity: "identity-rejected",
	oauth.OpRevoke:   "revoke-rejected",
}
