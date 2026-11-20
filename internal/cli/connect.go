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
	services  string
	noBrowser bool
	complete  string
}

func newConnectCmd(d deps) *cobra.Command {
	var f connectFlags
	cmd := &cobra.Command{
		Use:   "connect <provider> --as <alias> [--services gmail,drive,calendar] [--no-browser]",
		Short: "Connect an account through the provider's OAuth consent screen",
		Long: "Connect an account, or re-consent an existing alias for more services.\n\n" +
			"By default addn opens the consent screen in your browser and waits up to 5 minutes for\n" +
			"the redirect to a listener on 127.0.0.1.\n\n" +
			"Headless or over SSH, use two steps:\n" +
			"  addn connect google --as work --no-browser\n" +
			"      prints JSON with authorize_url; open it on any device and approve. The browser then\n" +
			"      lands on http://127.0.0.1:<port>/callback?code=…&state=…, a page that fails to load;\n" +
			"      copy the full URL from the address bar.\n" +
			"  addn connect --complete '<that URL>'\n" +
			"      finishes the connection. Each authorization works once, for 10 minutes.\n\n" +
			"--services picks services by command or provider id (drive or google-drive); without it,\n" +
			"addn asks for every service that has tools. Reconnecting asks for the union of the scopes\n" +
			"already granted and the new ones, and stores what the provider actually granted.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.complete != "" {
				if len(args) > 0 || f.alias != "" || f.services != "" || f.noBrowser {
					return usageError("--complete takes only the redirect URL")
				}
				return completeTwoStep(cmd, d, f.complete)
			}
			if len(args) != 1 {
				return usageError("connect needs a provider, e.g. `addn connect google --as work`")
			}
			if f.alias == "" {
				return usageError("connect needs --as <alias>")
			}
			return startConnect(cmd, d, args[0], f)
		},
	}
	cmd.Flags().StringVar(&f.alias, "as", "", "alias for the account, e.g. work")
	cmd.Flags().StringVar(&f.services, "services", "", "comma-separated services to authorize, e.g. gmail,drive,calendar")
	cmd.Flags().BoolVar(&f.noBrowser, "no-browser", false, "print the authorize URL and finish later with --complete")
	cmd.Flags().StringVar(&f.complete, "complete", "", "finish a --no-browser connect with the URL the browser landed on")
	return cmd
}

// connectTarget is everything both flows resolve before talking to the provider.
type connectTarget struct {
	provider *catalog.Provider
	client   oauth.Client
	alias    string
	scopes   []string
}

func startConnect(cmd *cobra.Command, d deps, providerID string, f connectFlags) error {
	ctx := cmd.Context()
	c, err := d.loadCatalog()
	if err != nil {
		return err
	}
	provider, err := identityRootProvider(c, providerID)
	if err != nil {
		return err
	}
	if err := vault.ValidateAccountName(provider.ID, f.alias); err != nil {
		return vaultError(err)
	}
	scopes, err := requestedScopes(c, provider, f.services)
	if err != nil {
		return err
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
	target.scopes = unionScopes(target.scopes, scopes)
	if f.noBrowser {
		return startTwoStep(cmd, d, s, target)
	}
	return connectViaLoopback(cmd, d, s, target)
}

// loadTarget loads the OAuth client and the scopes the alias already holds.
func loadTarget(ctx context.Context, s *vault.Store, provider *catalog.Provider, alias string) (connectTarget, error) {
	client, err := s.GetOAuthClient(ctx, provider.ID)
	if errors.Is(err, vault.ErrNotFound) {
		return connectTarget{}, &Error{Code: "no-oauth-client", Exit: ExitUsage, Message: fmt.Sprintf(
			"no OAuth client for %s; create a \"Desktop app\" client and run `addn client add %s --file client_secret.json`",
			provider.ID, provider.ID)}
	}
	if err != nil {
		return connectTarget{}, vaultError(err)
	}
	t := connectTarget{provider: provider, client: oauth.Client{ID: client.ClientID, Secret: client.ClientSecret}, alias: alias}
	existing, err := s.GetAccount(ctx, provider.ID, alias)
	if err == nil {
		t.scopes = existing.Scopes
	} else if !errors.Is(err, vault.ErrNotFound) {
		return connectTarget{}, vaultError(err)
	}
	return t, nil
}

func connectViaLoopback(cmd *cobra.Command, d deps, s *vault.Store, t connectTarget) error {
	lb, err := oauth.ListenLoopback(0)
	if err != nil {
		return &Error{Code: "loopback", Message: "listen on 127.0.0.1: " + err.Error(), Exit: ExitInternal}
	}
	defer lb.Close()
	auth, err := oauth.NewAuthorization(t.provider.OAuth2, lb.RedirectURI(), t.scopes)
	if err != nil {
		return err
	}
	authorizeURL := auth.URL(t.provider.OAuth2, t.client)
	if err := d.openBrowser(authorizeURL); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not open a browser. Open this URL to authorize addn:\n%s\n", authorizeURL)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), d.loopbackTimeout)
	defer cancel()
	cb, err := lb.Wait(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: "authorization-timeout", Exit: ExitUsage, Message: fmt.Sprintf(
			"no authorization arrived within %s; run connect again, or use --no-browser on a headless machine", d.loopbackTimeout)}
	}
	if err != nil {
		return err
	}
	code, err := cb.Verify(auth.State)
	if err != nil {
		return oauthError(err)
	}
	return finishConnect(cmd, d, s, t, auth, code)
}

func startTwoStep(cmd *cobra.Command, d deps, s *vault.Store, t connectTarget) error {
	port, err := oauth.FreeLoopbackPort()
	if err != nil {
		return &Error{Code: "loopback", Message: "pick a loopback port: " + err.Error(), Exit: ExitInternal}
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	auth, err := oauth.NewAuthorization(t.provider.OAuth2, redirectURI, t.scopes)
	if err != nil {
		return err
	}
	expires := d.now().Add(pendingAuthTTL).UTC().Truncate(time.Second)
	err = s.PutPendingAuth(cmd.Context(), vault.PendingAuth{
		State: auth.State, Provider: t.provider.ID, Alias: t.alias, Scopes: t.scopes,
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
	pending, err := s.TakePendingAuth(ctx, cb.State)
	switch {
	case errors.Is(err, vault.ErrNotFound):
		return &Error{Code: "unknown-state", Exit: ExitUsage,
			Message: "no pending authorization matches this URL's state; it was already used or never issued. Run `addn connect <provider> --as <alias> --no-browser` again"}
	case errors.Is(err, vault.ErrPendingExpired):
		return &Error{Code: "authorization-expired", Exit: ExitUsage,
			Message: "this authorization expired; run `addn connect <provider> --as <alias> --no-browser` again"}
	case err != nil:
		return vaultError(err)
	}
	if cb.RedirectURI != pending.RedirectURI {
		return &Error{Code: "redirect-mismatch", Exit: ExitUsage,
			Message: fmt.Sprintf("the URL is not the redirect this authorization expects (%s)", pending.RedirectURI)}
	}
	auth := oauth.Authorization{State: pending.State, Verifier: pending.Verifier, RedirectURI: pending.RedirectURI, Scopes: pending.Scopes}
	code, err := cb.Verify(pending.State)
	if err != nil {
		return oauthError(err)
	}
	provider, err := identityRootProvider(c, pending.Provider)
	if err != nil {
		return err
	}
	t, err := loadTarget(ctx, s, provider, pending.Alias)
	if err != nil {
		return err
	}
	t.scopes = pending.Scopes
	return finishConnect(cmd, d, s, t, auth, code)
}

type connectResult struct {
	Provider string   `json:"provider"`
	Alias    string   `json:"alias"`
	Identity string   `json:"identity"`
	Scopes   []string `json:"scopes"`
	Created  bool     `json:"created"`
}

// finishConnect exchanges the code, learns the identity, and stores the account, its credential and an audit row together.
func finishConnect(cmd *cobra.Command, d deps, s *vault.Store, t connectTarget, auth oauth.Authorization, code string) error {
	ctx := cmd.Context()
	tokens, err := oauth.Exchange(ctx, d.httpClient, t.provider, t.client, auth, code)
	if err != nil {
		return oauthError(err)
	}
	identity, err := oauth.FetchIdentity(ctx, d.httpClient, t.provider, tokens.AccessToken)
	if err != nil {
		return oauthError(err)
	}
	granted := tokens.Scopes
	if granted == nil {
		granted = auth.Scopes
	}
	result := connectResult{Provider: t.provider.ID, Alias: t.alias, Identity: identity, Scopes: granted}
	err = s.WithImmediateTx(ctx, func(tx *vault.Tx) error {
		acct, created, err := saveAccount(ctx, tx, result)
		if err != nil {
			return err
		}
		result.Created = created
		cred := vault.Credential{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, TokenType: tokens.TokenType}
		if cred.RefreshToken == "" && !created {
			if old, err := tx.GetCredential(ctx, acct.ID); err == nil {
				cred.RefreshToken = old.RefreshToken
			}
		}
		if tokens.ExpiresIn > 0 {
			cred.ExpiresAt = d.now().Add(tokens.ExpiresIn)
		}
		if err := tx.PutCredential(ctx, acct.ID, cred); err != nil {
			return err
		}
		return tx.Audit(ctx, vault.ConnectEvent{AccountID: acct.ID})
	})
	if err != nil {
		var typed *Error
		if errors.As(err, &typed) {
			return typed
		}
		return vaultError(err)
	}
	if err := writeJSON(cmd.OutOrStdout(), result); err != nil {
		return err
	}
	if missing := missingScopes(auth.Scopes, granted); len(missing) > 0 {
		return &Error{Code: "scopes-not-granted", Exit: ExitNeedsConnect, Message: fmt.Sprintf(
			"the account was saved, but %s did not grant: %s; connect again and allow them on the consent screen",
			t.provider.ID, strings.Join(missing, " "))}
	}
	return nil
}

// saveAccount creates the alias, or updates it when it is the same identity re-consenting.
func saveAccount(ctx context.Context, tx *vault.Tx, r connectResult) (vault.Account, bool, error) {
	existing, err := tx.GetAccount(ctx, r.Provider, r.Alias)
	if errors.Is(err, vault.ErrNotFound) {
		acct, err := tx.CreateAccount(ctx, vault.Account{Provider: r.Provider, Alias: r.Alias, Identity: r.Identity, Scopes: r.Scopes})
		return acct, true, err
	}
	if err != nil {
		return vault.Account{}, false, err
	}
	if existing.Identity != "" && existing.Identity != r.Identity {
		return vault.Account{}, false, &Error{Code: "identity-mismatch", Exit: ExitUsage, Message: fmt.Sprintf(
			"%s:%s is %s, but the consent was given as %s; nothing was changed. Connect that account under another alias",
			r.Provider, r.Alias, existing.Identity, r.Identity)}
	}
	existing.Identity, existing.Scopes, existing.Status = r.Identity, r.Scopes, vault.AccountActive
	acct, err := tx.UpdateAccount(ctx, existing)
	return acct, false, err
}

// oauthError maps engine failures to the exit codes: 2 for a bad paste or state, 4 for a provider refusal, 5 for the network or a provider 5xx.
func oauthError(err error) error {
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
	return &Error{Code: "internal", Message: err.Error(), Exit: ExitInternal}
}

var providerErrorCodes = map[string]string{
	"token exchange":  "token-exchange-rejected",
	"identity lookup": "identity-rejected",
	"revoke":          "revoke-rejected",
}
