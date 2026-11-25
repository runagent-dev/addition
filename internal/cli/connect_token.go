package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/oauth"
	"github.com/runagent-dev/addition/internal/proxy"
	"github.com/runagent-dev/addition/internal/vault"
)

// maxTokenBytes bounds the line read for a token; real tokens are far shorter.
const maxTokenBytes = 16 << 10

// verifyTimeout bounds one token check, from the request to the last byte of the identity response.
var verifyTimeout = 15 * time.Second

// maxIdentityBody bounds the identity response read to learn who a token belongs to.
const maxIdentityBody = 1 << 20

// connectWithToken stores an account for provider from a token the user created. The token is
// read, shape-checked and proved with one GET through the request engine before anything is stored.
func connectWithToken(cmd *cobra.Command, d deps, provider *catalog.Provider, f connectFlags) error {
	ctx := cmd.Context()
	s, err := d.openVault(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	token, err := readToken(cmd, d, provider, f)
	if err != nil {
		return err
	}
	if err := checkTokenShape(provider.APIKey, token); err != nil {
		return err
	}
	who, err := verifyToken(ctx, d, provider, f.alias, token)
	if err != nil {
		return err
	}
	result, err := storeTokenAccount(ctx, s, provider, f.alias, token, who)
	if err != nil {
		return commitError(err)
	}
	return writeJSON(cmd.OutOrStdout(), result)
}

// readToken gets the token from standard input, the named environment variable, or a prompt.
// Without a flag it asks only a person at a terminal, and never when input is piped.
func readToken(cmd *cobra.Command, d deps, provider *catalog.Provider, f connectFlags) (string, error) {
	switch {
	case f.tokenStdin:
		return readTokenLine(cmd.Context(), cmd.InOrStdin())
	case f.tokenEnv != "":
		value, set := d.lookupEnv(f.tokenEnv)
		if !set {
			return "", &Error{Code: "token-env-unset", Exit: ExitUsage, Message: "the environment variable named by --token-env is not set"}
		}
		return strings.TrimSpace(value), nil
	case d.prompter != nil && d.prompter.Interactive():
		return promptToken(cmd, d.prompter, provider, f.alias)
	}
	return "", tokenRequired(provider, f.alias)
}

func tokenRequired(provider *catalog.Provider, alias string) *Error {
	key := provider.APIKey
	env := "NAME"
	if key.EnvVar != "" {
		env = key.EnvVar
	}
	return &Error{Code: "token-required", Exit: ExitUsage, Message: fmt.Sprintf(
		"no token was given and there is no terminal to ask on; create one at %s, then run `addition connect %s --as %s --token-stdin` with it on standard input, or `--token-env %s` with it in that environment variable",
		key.CreateURL, provider.ID, alias, env)}
}

// promptToken explains where to create the token on stderr, then reads it without echo.
func promptToken(cmd *cobra.Command, prompter secretPrompter, provider *catalog.Provider, alias string) (string, error) {
	key, out := provider.APIKey, cmd.ErrOrStderr()
	fmt.Fprintf(out, "Create a token for %s at %s\n", provider.Name, key.CreateURL)
	for _, step := range key.Instructions {
		fmt.Fprintf(out, "  - %s\n", step)
	}
	fmt.Fprint(out, "Paste the token (it is not shown): ")
	secret, err := prompter.ReadSecret()
	fmt.Fprintln(out)
	if err != nil {
		return "", tokenRequired(provider, alias)
	}
	return strings.TrimSpace(secret), nil
}

// readTokenLine reads the first line of r, trimmed. Cancelling ctx ends the wait; the read ends with the process.
func readTokenLine(ctx context.Context, r io.Reader) (string, error) {
	type read struct {
		line string
		err  error
	}
	done := make(chan read, 1)
	go func() {
		line, err := bufio.NewReader(io.LimitReader(r, maxTokenBytes)).ReadString('\n')
		done <- read{line, err}
	}()
	select {
	case got := <-done:
		if got.err != nil && !errors.Is(got.err, io.EOF) {
			return "", &Error{Code: "empty-token", Exit: ExitUsage, Message: "standard input could not be read"}
		}
		if len(got.line) >= maxTokenBytes && !strings.HasSuffix(got.line, "\n") {
			return "", badToken()
		}
		return strings.TrimSpace(got.line), nil
	case <-ctx.Done():
		return "", interrupted()
	}
}

func badToken() *Error {
	return &Error{Code: "bad-token", Exit: ExitUsage, Message: "the token must be one line of printable characters without spaces"}
}

// checkTokenShape refuses a token that is empty, could not be an HTTP header value, or lacks the
// prefix the app's tokens start with. No message repeats any part of the token.
func checkTokenShape(key *catalog.APIKey, token string) error {
	if token == "" {
		return &Error{Code: "empty-token", Exit: ExitUsage, Message: "the token is empty"}
	}
	if strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r >= 0x7f }) {
		return badToken()
	}
	if key.TokenPrefix != "" && !strings.HasPrefix(token, key.TokenPrefix) {
		return &Error{Code: "token-prefix-mismatch", Exit: ExitUsage, Message: fmt.Sprintf(
			"this app's tokens start with %q and the token given does not, so nothing was sent; create one at %s", key.TokenPrefix, key.CreateURL)}
	}
	return nil
}

// verifyToken proves token with one GET of the app's identity endpoint, or its probe path, through
// the engine `addition call` uses: the host and path allow-lists, the echo guard and no redirects apply.
func verifyToken(ctx context.Context, d deps, provider *catalog.Provider, alias, token string) (oauth.Identity, error) {
	resp, err := sendVerification(ctx, d, provider, alias, token)
	if err != nil {
		return oauth.Identity{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.Status == http.StatusUnauthorized || resp.Status == http.StatusForbidden:
		return oauth.Identity{}, invalidToken(provider, alias)
	case resp.Status < 200 || resp.Status > 299:
		return oauth.Identity{}, oauthError(&oauth.ProviderError{Op: oauth.OpIdentity, Status: resp.Status})
	}
	identity := provider.APIKey.Identity
	if identity == nil {
		return oauth.Identity{}, nil
	}
	return readIdentity(ctx, resp, identity, token)
}

func sendVerification(ctx context.Context, d deps, provider *catalog.Provider, alias, token string) (*proxy.Response, error) {
	target, err := verificationURL(provider)
	if err != nil {
		return nil, callRequestError(err)
	}
	engine := &proxy.Engine{
		Client: d.httpClient, API: provider.API, AuthMethod: catalog.MethodAPIKey,
		AllowCleartext: d.loopbackProviders, Now: d.now, Sleep: d.sleep,
	}
	header := http.Header{"Accept": {"application/json"}, "User-Agent": {"addition/" + currentBuild().Version}}
	resp, err := engine.Do(ctx, proxy.Request{Method: http.MethodGet, URL: target, Header: header}, token)
	if err == nil {
		return resp, nil
	}
	if attempted, sent := errors.AsType[*proxy.Attempted](err); sent && attempted.Status == http.StatusUnauthorized {
		return nil, invalidToken(provider, alias)
	}
	return nil, verificationFailure(ctx, err)
}

// verificationFailure maps a failure to send the check to addition's own error; no transport text leaves.
func verificationFailure(ctx context.Context, err error) error {
	if reqErr, ok := errors.AsType[*proxy.RequestError](err); ok {
		return callRequestError(reqErr)
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return interrupted()
	}
	return networkFailure()
}

func invalidToken(provider *catalog.Provider, alias string) *Error {
	return &Error{Code: "invalid-token", Exit: ExitNeedsConnect, Message: fmt.Sprintf(
		"%s refused the token, so nothing was stored; create a token at %s, then run `addition connect %s --as %s --token-stdin` with it",
		provider.Name, provider.APIKey.CreateURL, provider.ID, alias)}
}

// readIdentity reads who the token belongs to from a 2xx identity response, withholding a response
// that repeats the token in any field.
func readIdentity(ctx context.Context, resp *proxy.Response, identity *catalog.APIKeyIdentity, token string) (oauth.Identity, error) {
	if hasContentCoding(resp.Header) {
		return oauth.Identity{}, unsupportedEncoding(resp.Status)
	}
	guard := proxy.NewEchoGuard(resp.Tokens)
	body, err := io.ReadAll(io.LimitReader(guard.Reader(io.LimitReader(resp.Body, maxIdentityBody+int64(guard.Lookahead()))), maxIdentityBody))
	switch {
	case errors.Is(err, proxy.ErrCredentialEcho):
		return oauth.Identity{}, credentialEcho()
	case err != nil && (ctx.Err() != nil || errors.Is(err, context.Canceled)):
		return oauth.Identity{}, interrupted()
	case err != nil:
		return oauth.Identity{}, networkFailure()
	}
	who, err := oauth.ParseIdentity(resp.Status, body, identity.SubjectField, identity.Field)
	if err != nil {
		return oauth.Identity{}, oauthError(err)
	}
	if strings.Contains(who.Subject, token) || strings.Contains(who.Display, token) {
		return oauth.Identity{}, credentialEcho()
	}
	return who, nil
}

// storeTokenAccount creates the account, or rebinds the alias to the same subject, and replaces
// its credential with the token, in one transaction with the connect audit row.
func storeTokenAccount(ctx context.Context, s *vault.Store, provider *catalog.Provider, alias, token string, who oauth.Identity) (connectResult, error) {
	result := connectResult{Provider: provider.ID, Alias: alias, Identity: who.Display, Scopes: []string{}, Method: catalog.MethodAPIKey}
	err := s.WithImmediateTx(ctx, func(tx *vault.Tx) error {
		acct, created, err := bindTokenAccount(ctx, tx, provider, alias, who)
		if err != nil {
			return err
		}
		result.Created = created
		if err := tx.PutCredential(ctx, acct.ID, vault.Credential{Kind: vault.KindAPIKey, AccessToken: token}); err != nil {
			return err
		}
		return tx.Audit(ctx, vault.ConnectEvent{AccountID: acct.ID})
	})
	return result, err
}

// bindTokenAccount creates the alias, or makes the existing one active again if the token belongs to its subject.
func bindTokenAccount(ctx context.Context, tx *vault.Tx, provider *catalog.Provider, alias string, who oauth.Identity) (acct vault.Account, created bool, err error) {
	acct, err = tx.GetAccount(ctx, provider.ID, alias)
	if errors.Is(err, vault.ErrNotFound) {
		acct, err = tx.CreateAccount(ctx, vault.Account{Provider: provider.ID, Alias: alias, Identity: who.Display, Subject: who.Subject, Scopes: []string{}})
		return acct, true, err
	}
	if err != nil {
		return vault.Account{}, false, err
	}
	if !matchesAccount(acct, who.Subject, who.Display) {
		return vault.Account{}, false, tokenBelongsToAnother(provider, acct)
	}
	acct.Identity, acct.Subject, acct.Scopes, acct.Status = who.Display, who.Subject, []string{}, vault.AccountActive
	acct, err = tx.UpdateAccount(ctx, acct)
	return acct, false, err
}

func tokenBelongsToAnother(provider *catalog.Provider, existing vault.Account) *Error {
	return &Error{Code: "identity-mismatch", Exit: ExitUsage, Message: fmt.Sprintf(
		"%s:%s is %s, but the token belongs to another account; nothing was changed. Connect that account under another alias",
		provider.ID, existing.Alias, existing.Identity)}
}

// reconnectCommand is what replaces the credential of an account: the token command for an API key.
func reconnectCommand(acct vault.Account, kind string) string {
	command := fmt.Sprintf("addition connect %s --as %s", acct.Provider, acct.Alias)
	if kind == vault.KindAPIKey {
		command += " --token-stdin"
	}
	return command
}

// verificationURL is where a token is checked: the identity path, else the probe path, resolved against the allow-lists.
func verificationURL(provider *catalog.Provider) (*url.URL, error) {
	path := provider.API.ProbePath
	if identity := provider.APIKey.Identity; identity != nil {
		path = identity.Path
	}
	return proxy.ResolveURL(provider.API, path, nil)
}
