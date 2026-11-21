package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/oauth"
	"github.com/runagent-dev/addition/internal/proxy"
	"github.com/runagent-dev/addition/internal/vault"
)

const (
	defaultCallProvider = "google"
	maxErrorBody        = 1 << 20
	sniffLength         = 512
)

// maxRequestBody caps a request body read from a file, stdin or a flag.
var maxRequestBody int64 = 64 << 20

type callFlags struct {
	account, provider string
	query, headers    []string
	data, output      string
	dataSet, force    bool
}

func newCallCmd(d deps) *cobra.Command {
	var f callFlags
	cmd := &cobra.Command{
		Use:   "call [-a <alias>] <METHOD> <path> [--query k=v]... [--data @file.json|-|'<json>'] [-H 'Name: value']... [-o file] [--force]",
		Short: "Call any provider API with a connected account's token",
		Long: "Send one authenticated request to the provider's API and print the response body.\n\n" +
			"<path> is absolute from the API host (/calendar/v3/calendars/primary/events) or an https URL\n" +
			"on one of the provider's allowed hosts (see api.allowed_hosts in its YAML); nothing is sent to\n" +
			"any other host. addn sets Authorization itself, refreshes an expiring token first, renews it\n" +
			"once on a 401, and waits out 429 responses. Redirects are never followed.\n\n" +
			"A 2xx text or JSON body goes to stdout verbatim. A binary body needs -o <file>, which writes it\n" +
			"privately and prints {status, content_type, bytes, path}; an existing file needs --force.\n" +
			"For any other status the upstream body goes to stdout and a JSON envelope on stderr carries\n" +
			"code upstream-error and the status. Exit codes: 4 for a 4xx or 3xx, 5 for a 5xx or the network,\n" +
			"3 when the account must be reconnected.\n\n" +
			"-a is optional when one account is connected or a default is set; --provider names the provider\n" +
			"when an alias exists under several.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			f.dataSet = cmd.Flags().Changed("data")
			return runCall(cmd, d, args[0], args[1], f)
		},
	}
	flags := cmd.Flags()
	flags.StringVarP(&f.account, "account", "a", "", "alias of the account to use")
	flags.StringVar(&f.provider, "provider", "", "provider of the account, when the alias is not unique")
	flags.StringArrayVar(&f.query, "query", nil, "query parameter k=v; repeatable")
	flags.StringVar(&f.data, "data", "", "request body: @file, - for stdin, or the body itself")
	flags.StringArrayVarP(&f.headers, "header", "H", nil, "extra request header 'Name: value'; repeatable")
	flags.StringVarP(&f.output, "output", "o", "", "write a 2xx response body to this file")
	flags.BoolVar(&f.force, "force", false, "let -o replace an existing file")
	return cmd
}

func runCall(cmd *cobra.Command, d deps, rawMethod, rawPath string, f callFlags) error {
	ctx := cmd.Context()
	method, err := proxy.ParseMethod(rawMethod)
	if err != nil {
		return callRequestError(err)
	}
	header, err := proxy.ParseHeaders(f.headers)
	if err != nil {
		return callRequestError(err)
	}
	body, err := readRequestBody(cmd.InOrStdin(), f)
	if err != nil {
		return err
	}
	var out *outputFile
	if f.output != "" {
		if out, err = checkOutputFile(f.output, f.force); err != nil {
			return err
		}
	}
	a, err := openAccountSession(ctx, d)
	if err != nil {
		return err
	}
	defer a.store.Close()
	acct, err := a.callAccount(ctx, f.account, f.provider)
	if err != nil {
		return err
	}
	provider, _ := a.catalog.Provider(acct.Provider)
	target, err := proxy.ResolveURL(provider.API, rawPath, f.query)
	if err != nil {
		return callRequestError(err)
	}
	if header.Get("User-Agent") == "" {
		header.Set("User-Agent", "addn/"+Version)
	}

	tokens := &proxy.TokenManager{Store: a.store, Client: d.httpClient, Provider: provider, Account: acct, Now: d.now}
	token, err := tokens.Access(ctx)
	if err != nil {
		return callFailure(acct, err)
	}
	engine := &proxy.Engine{Client: d.httpClient, API: provider.API, Renewer: tokens, Now: d.now, Sleep: d.sleep}
	resp, err := engine.Do(ctx, proxy.Request{Method: method, URL: target, Header: header, Body: body}, token)
	if status, sent := sentStatus(err); err != nil && sent {
		recordCall(ctx, cmd, a.store, acct, method, status)
	}
	if err != nil {
		return callFailure(acct, err)
	}
	defer resp.Body.Close()
	recordCall(ctx, cmd, a.store, acct, method, resp.Status)
	return deliver(cmd, resp, out)
}

// sentStatus is the status to audit for a request that failed after being sent: 0 for no response.
func sentStatus(err error) (status int, sent bool) {
	var auth *proxy.AuthFailure
	var network *proxy.NetworkError
	switch {
	case errors.As(err, &auth):
		return auth.Status, true
	case errors.As(err, &network):
		return 0, true
	}
	return 0, false
}

// recordCall audits a finished request. The call already happened, so a failed write is a stderr notice, not an error.
func recordCall(ctx context.Context, cmd *cobra.Command, s *vault.Store, acct vault.Account, method string, status int) {
	if err := s.Audit(ctx, vault.CallEvent{AccountID: acct.ID, Method: method, Status: status}); err != nil {
		_ = writeJSON(cmd.ErrOrStderr(), map[string]string{"notice": "audit-failed"})
	}
}

func callRequestError(err error) error {
	if reqErr, ok := errors.AsType[*proxy.RequestError](err); ok {
		return &Error{Code: reqErr.Code, Message: reqErr.Message, Exit: ExitUsage}
	}
	return &Error{Code: "internal", Message: "the request could not be prepared", Exit: ExitInternal}
}

// callFailure maps token and transport failures to exit codes. Every message is addn's own.
func callFailure(acct vault.Account, err error) error {
	var refused *oauth.ProviderError
	var tokenNet *oauth.NetworkError
	var apiNet *proxy.NetworkError
	switch {
	case errors.Is(err, proxy.ErrNeedsReauth), errors.Is(err, proxy.ErrNoRefreshToken):
		return &Error{Code: "needs-reauth", Exit: ExitNeedsConnect, Message: fmt.Sprintf(
			"%s:%s must be reconnected; run `addn connect %s --as %s`", acct.Provider, acct.Alias, acct.Provider, acct.Alias)}
	case errors.Is(err, proxy.ErrNoCredential):
		return notConnected(accountRef{provider: acct.Provider, alias: acct.Alias})
	case errors.Is(err, proxy.ErrNoOAuthClient):
		return &Error{Code: "no-oauth-client", Exit: ExitUsage, Message: fmt.Sprintf(
			"no OAuth client for %s; run `addn client add %s --file client_secret.json`", acct.Provider, acct.Provider)}
	case errors.As(err, &refused):
		return &Error{Code: "refresh-rejected", Message: refused.Error(), Exit: upstreamExit(refused.Status)}
	case errors.As(err, &tokenNet), errors.As(err, &apiNet):
		return &Error{Code: "network", Message: err.Error(), Exit: ExitUpstreamServer}
	}
	if reqErr, ok := errors.AsType[*proxy.RequestError](err); ok {
		return callRequestError(reqErr)
	}
	return &Error{Code: "internal", Message: "the call failed inside addn", Exit: ExitInternal}
}

func upstreamExit(status int) int {
	if status >= 500 {
		return ExitUpstreamServer
	}
	return ExitUpstreamClient
}

// callAccount picks the account a call uses: the alias, else the provider's default, else its only account.
func (a *accountSession) callAccount(ctx context.Context, alias, providerFlag string) (vault.Account, error) {
	root := ""
	if providerFlag != "" {
		var err error
		if root, err = a.catalog.IdentityRoot(providerFlag); err != nil {
			return vault.Account{}, &Error{Code: "unknown-provider", Exit: ExitUsage,
				Message: fmt.Sprintf("unknown provider %q (see `addn providers`)", providerFlag)}
		}
	}
	candidates, err := a.store.ListAccounts(ctx, root)
	if err != nil {
		return vault.Account{}, vaultError(err)
	}
	if alias != "" {
		return a.byAlias(candidates, alias, root)
	}
	return a.byDefault(ctx, candidates, root)
}

func (a *accountSession) byAlias(candidates []vault.Account, alias, root string) (vault.Account, error) {
	var matches []vault.Account
	for _, acct := range candidates {
		if acct.Alias == alias {
			matches = append(matches, acct)
		}
	}
	switch len(matches) {
	case 0:
		return vault.Account{}, notConnected(accountRef{provider: orDefault(root), alias: alias})
	case 1:
		return matches[0], nil
	}
	return vault.Account{}, &Error{Code: "ambiguous-account", Exit: ExitUsage,
		Message: fmt.Sprintf("the alias %q exists under several providers; pass --provider", alias)}
}

func (a *accountSession) byDefault(ctx context.Context, candidates []vault.Account, root string) (vault.Account, error) {
	if len(candidates) == 0 {
		return vault.Account{}, &Error{Code: "not-connected", Exit: ExitNeedsConnect,
			Message: fmt.Sprintf("no account is connected; run `addn connect %s --as <alias>`", orDefault(root))}
	}
	if root == "" && !allSameProvider(candidates) {
		return vault.Account{}, &Error{Code: "ambiguous-account", Exit: ExitUsage, Message: "accounts of several providers are connected; pass --provider and -a"}
	}
	provider := candidates[0].Provider
	if def, err := a.store.DefaultAccount(ctx, provider); err == nil {
		for _, acct := range candidates {
			if acct.Alias == def {
				return acct, nil
			}
		}
	} else if !errors.Is(err, vault.ErrNotFound) {
		return vault.Account{}, vaultError(err)
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	aliases := make([]string, len(candidates))
	for i, acct := range candidates {
		aliases[i] = acct.Alias
	}
	return vault.Account{}, &Error{Code: "account-required", Exit: ExitUsage, Message: fmt.Sprintf(
		"several accounts are connected (%s); pass -a <alias> or set a default with `addn accounts default %s:<alias>`",
		strings.Join(aliases, ", "), provider)}
}

func allSameProvider(accts []vault.Account) bool {
	for _, acct := range accts {
		if acct.Provider != accts[0].Provider {
			return false
		}
	}
	return true
}

func orDefault(provider string) string {
	if provider == "" {
		return defaultCallProvider
	}
	return provider
}

func readRequestBody(stdin io.Reader, f callFlags) ([]byte, error) {
	switch {
	case !f.dataSet:
		return nil, nil
	case f.data == "-":
		return readCapped(stdin)
	case strings.HasPrefix(f.data, "@"):
		return readBodyFile(f.data[1:])
	}
	return capBody([]byte(f.data))
}

func readBodyFile(path string) ([]byte, error) {
	if path == "" {
		return nil, &Error{Code: "bad-data", Exit: ExitUsage, Message: "--data @ needs a file name"}
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, &Error{Code: "bad-data", Exit: ExitUsage, Message: fmt.Sprintf("read the --data file: %v", err)}
	}
	defer file.Close()
	return readCapped(file)
}

func readCapped(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxRequestBody+1))
	if err != nil {
		return nil, &Error{Code: "bad-data", Exit: ExitUsage, Message: fmt.Sprintf("read the request body: %v", err)}
	}
	return capBody(data)
}

func capBody(data []byte) ([]byte, error) {
	if int64(len(data)) > maxRequestBody {
		return nil, &Error{Code: "body-too-large", Exit: ExitUsage, Message: fmt.Sprintf("the request body is over %d bytes", maxRequestBody)}
	}
	return data, nil
}

// deliver writes the final response: a success body to stdout or the -o file, anything else as an upstream failure.
func deliver(cmd *cobra.Command, resp *proxy.Response, out *outputFile) error {
	if resp.Status < 200 || resp.Status > 299 {
		return deliverFailure(cmd, resp)
	}
	if out != nil {
		return out.save(cmd, resp)
	}
	body := bufio.NewReaderSize(resp.Body, sniffLength)
	head, err := body.Peek(sniffLength)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return networkFailure()
	}
	if !isTextual(resp.Header.Get("Content-Type"), head) {
		return &Error{Code: "binary-response", Exit: ExitUsage,
			Message: "the response is not text or JSON; repeat the call with -o <file> to save it"}
	}
	return copyBody(cmd.OutOrStdout(), body)
}

// deliverFailure prints the upstream body, capped, and reports only the status. A redirect's body is never read.
func deliverFailure(cmd *cobra.Command, resp *proxy.Response) error {
	if resp.Status < 300 || resp.Status > 399 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		if _, err := cmd.OutOrStdout().Write(body); err != nil {
			return err
		}
	}
	return &Error{Code: "upstream-error", Message: fmt.Sprintf("HTTP %d", resp.Status), Exit: upstreamExit(resp.Status)}
}

func networkFailure() *Error {
	return &Error{Code: "network", Message: (&proxy.NetworkError{}).Error(), Exit: ExitUpstreamServer}
}

// copyBody streams src to dst, telling a broken upstream from a broken stdout.
func copyBody(dst io.Writer, src io.Reader) error {
	watched := &failureReader{r: src}
	_, err := io.Copy(dst, watched)
	if watched.err != nil {
		return networkFailure()
	}
	return err
}

type failureReader struct {
	r   io.Reader
	err error
}

func (f *failureReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		f.err = err
	}
	return n, err
}

// isTextual reports whether a response body can go to stdout: text, JSON or XML by its
// Content-Type, or by sniffing its first bytes when the type is missing or unreadable.
func isTextual(contentType string, head []byte) bool {
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		switch {
		case strings.HasPrefix(mediaType, "text/"), strings.HasSuffix(mediaType, "+json"), strings.HasSuffix(mediaType, "+xml"):
			return true
		}
		switch mediaType {
		case "application/json", "application/xml", "application/x-ndjson", "application/javascript", "application/x-www-form-urlencoded":
			return true
		}
		return false
	}
	return strings.HasPrefix(http.DetectContentType(head), "text/")
}
