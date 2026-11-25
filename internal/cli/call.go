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
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/runagent-dev/addition/internal/catalog"
	"github.com/runagent-dev/addition/internal/oauth"
	"github.com/runagent-dev/addition/internal/proxy"
	"github.com/runagent-dev/addition/internal/vault"
)

const (
	maxErrorBody = 1 << 20
	sniffLength  = 512
)

// maxRequestBody caps a request body read from a file, stdin or a flag.
var maxRequestBody int64 = 64 << 20

type callFlags struct {
	account        string
	query, headers []string
	data, output   string
	dataSet, force bool
}

func newCallCmd(d deps) *cobra.Command {
	var f callFlags
	cmd := &cobra.Command{
		Use:   "call <app> [-a <alias>] <METHOD> <path> [--query k=v]... [--data @file.json|-|'<json>'] [-H 'Name: value']... [-o file] [--force]",
		Short: "Call an app's API with a connected account's token",
		Long: "Send one authenticated request to an app's API and print the response body.\n\n" +
			"<app> is a provider id or toolkit command, e.g. gmail, drive (google-drive) or google-chat:\n" +
			"addition call google-chat -a work GET /v1/spaces\n\n" +
			"<path> is absolute from the app's API host (/calendar/v3/calendars/primary/events) or an https URL\n" +
			"on one of the app's allowed hosts, and its path must start with one of the app's allowed prefixes\n" +
			"(api.allowed_hosts and api.allowed_path_prefixes in its YAML; path-not-allowed otherwise). Nothing is\n" +
			"sent to any other host or route, so an app's token never reaches another app's API.\n" +
			"addition sets Authorization itself, refreshes an expiring token first, renews it\n" +
			"once on a 401, and waits out 429 responses. Redirects are never followed.\n\n" +
			"A 2xx text or JSON body goes to stdout verbatim. A binary body needs -o <file>, which writes it\n" +
			"privately and prints {status, content_type, bytes, path}; an existing file needs --force.\n" +
			"For any other status the upstream body goes to stdout and a JSON envelope on stderr carries\n" +
			"code upstream-error and the status. Exit codes: 4 for a 4xx or 3xx, 5 for a 5xx or the network,\n" +
			"3 when the account must be reconnected. A response that repeats the access token is withheld\n" +
			"(credential-echo, exit 5), and so is one with any content coding\n" +
			"(unsupported-encoding).\n\n" +
			"-a is optional when the app has one account connected or a default is set.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && isMethod(args[0]) {
				return appMissing(cmd, d)
			}
			if len(args) != 3 {
				return usageError(callForm)
			}
			f.dataSet = cmd.Flags().Changed("data")
			return runCall(cmd, d, args[0], args[1], args[2], f)
		},
	}
	flags := cmd.Flags()
	flags.StringVarP(&f.account, "account", "a", "", "alias of the account to use")
	flags.StringArrayVar(&f.query, "query", nil, "query parameter k=v; repeatable")
	flags.StringVar(&f.data, "data", "", "request body: @file, - for stdin, or the body itself")
	flags.StringArrayVarP(&f.headers, "header", "H", nil, "extra request header 'Name: value'; repeatable")
	flags.StringVarP(&f.output, "output", "o", "", "write a 2xx response body to this file")
	flags.BoolVar(&f.force, "force", false, "let -o replace an existing file")
	return cmd
}

// callRequest is everything about a call that can be checked before the vault is opened.
type callRequest struct {
	method string
	header http.Header
	body   []byte
	out    *outputFile
}

func prepareCall(ctx context.Context, stdin io.Reader, rawMethod string, f callFlags) (callRequest, error) {
	method, err := proxy.ParseMethod(rawMethod)
	if err != nil {
		return callRequest{}, callRequestError(err)
	}
	header, err := proxy.ParseHeaders(f.headers)
	if err != nil {
		return callRequest{}, callRequestError(err)
	}
	if header.Get("User-Agent") == "" {
		header.Set("User-Agent", "addition/"+currentBuild().Version)
	}
	body, err := readRequestBody(ctx, stdin, f)
	if err != nil {
		return callRequest{}, err
	}
	req := callRequest{method: method, header: header, body: body}
	if f.output != "" {
		if req.out, err = openOutputFile(f.output, f.force); err != nil {
			return callRequest{}, err
		}
	}
	return req, nil
}

const callForm = "call needs <app> <METHOD> <path>, e.g. `addition call gmail GET /gmail/v1/users/me/profile`"

func isMethod(arg string) bool {
	_, err := proxy.ParseMethod(arg)
	return err == nil
}

// appMissing reports a call that starts with a method, so names no app. The message is fixed:
// it never repeats an argument, which may hold a secret.
func appMissing(cmd *cobra.Command, d deps) error {
	return &Error{Code: "app-required", Exit: ExitUsage, Message: "call needs the app first: `addition call <app> <METHOD> <path>`; " +
		connectedAppsHint(cmd.Context(), d)}
}

// connectedAppsHint names the apps with an account, or says none is connected, for a usage message.
func connectedAppsHint(ctx context.Context, d deps) string {
	none := "no app is connected yet; run `addition connect <app> --as <alias>`"
	a, err := openAccountSession(ctx, d)
	if err != nil {
		return none
	}
	defer a.store.Close()
	accts, err := a.store.ListAccounts(ctx, "")
	if err != nil {
		return none
	}
	var apps []string
	for _, acct := range accts {
		if a.catalog.Connectable(acct.Provider) && !slices.Contains(apps, acct.Provider) {
			apps = append(apps, acct.Provider)
		}
	}
	if len(apps) == 0 {
		return none
	}
	return "connected apps: " + strings.Join(apps, ", ")
}

func runCall(cmd *cobra.Command, d deps, appName, rawMethod, rawPath string, f callFlags) error {
	ctx := cmd.Context()
	req, err := prepareCall(ctx, cmd.InOrStdin(), rawMethod, f)
	if err != nil {
		return err
	}
	defer req.out.discard()
	a, err := openAccountSession(ctx, d)
	if err != nil {
		return err
	}
	defer a.store.Close()
	provider, err := a.app(ctx, appName, f.account)
	if err != nil {
		return err
	}
	acct, err := a.callAccount(ctx, provider, f.account)
	if err != nil {
		return err
	}
	target, err := proxy.ResolveURL(provider.API, rawPath, f.query)
	if err != nil {
		return callRequestError(err)
	}

	tokens := &proxy.TokenManager{Store: a.store, Client: d.httpClient, Provider: provider, Account: acct, Now: d.now}
	token, err := tokens.Access(ctx)
	if err != nil {
		return callFailure(acct, provider, err)
	}
	engine := &proxy.Engine{
		Client: d.httpClient, API: provider.API, AuthMethod: tokens.AuthMethod(), AllowCleartext: d.loopbackProviders,
		Renewer: tokens, Now: d.now, Sleep: d.sleep,
	}
	resp, err := engine.Do(ctx, proxy.Request{Method: req.method, URL: target, Header: req.header, Body: req.body}, token)
	if err != nil {
		if attempted, sent := errors.AsType[*proxy.Attempted](err); sent {
			recordCall(cmd, a.store, acct, req.method, attempted.Status)
		}
		return callFailure(acct, provider, err)
	}
	defer resp.Body.Close()
	recordCall(cmd, a.store, acct, req.method, resp.Status)
	return sealCallError(ctx, deliver(cmd, req.method, resp, req.out))
}

// sealCallError lets only addition's own errors out of a call whose request was sent, so no Go or upstream error text can reach the user.
func sealCallError(ctx context.Context, err error) error {
	if _, typed := errors.AsType[*Error](err); typed || err == nil {
		return err
	}
	if ctx.Err() != nil {
		return interrupted()
	}
	return networkFailure()
}

// auditTimeout bounds a call audit write made after the command's own context may be cancelled.
var auditTimeout = 5 * time.Second

// recordCall audits a request that was sent. The call already happened, so it ignores cancellation
// of the command, and a failed write is a stderr notice, not an error.
func recordCall(cmd *cobra.Command, s *vault.Store, acct vault.Account, method string, status int) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(cmd.Context()), auditTimeout)
	defer cancel()
	if err := s.AuditWithinDeadline(ctx, vault.CallEvent{AccountID: acct.ID, Method: method, Status: status}); err != nil {
		_ = writeJSON(cmd.ErrOrStderr(), map[string]string{"notice": "audit-failed"})
	}
}

func callRequestError(err error) error {
	if reqErr, ok := errors.AsType[*proxy.RequestError](err); ok {
		return &Error{Code: reqErr.Code, Message: reqErr.Message, Exit: ExitUsage}
	}
	return &Error{Code: "internal", Message: "the request could not be prepared", Exit: ExitInternal}
}

// callFailure maps token and transport failures to exit codes. Every message is addition's own.
func callFailure(acct vault.Account, provider *catalog.Provider, err error) error {
	var refused *oauth.ProviderError
	var tokenNet *oauth.NetworkError
	var apiNet *proxy.NetworkError
	switch {
	case errors.Is(err, context.Canceled):
		return interrupted()
	case errors.Is(err, proxy.ErrTokenRejected):
		return &Error{Code: "needs-reauth", Exit: ExitNeedsConnect, Message: fmt.Sprintf(
			"the provider rejected the token of %s:%s; create a new one and run `%s`", acct.Provider, acct.Alias, reconnectCommand(provider, acct, vault.KindAPIKey))}
	case errors.Is(err, proxy.ErrNeedsReauth), errors.Is(err, proxy.ErrNoRefreshToken):
		return &Error{Code: "needs-reauth", Exit: ExitNeedsConnect, Message: fmt.Sprintf(
			"%s:%s must be reconnected; run `%s`", acct.Provider, acct.Alias, reconnectCommand(provider, acct, vault.KindOAuth2))}
	case errors.Is(err, proxy.ErrAccountChanged):
		return &Error{Code: "account-changed", Exit: ExitUsage, Message: fmt.Sprintf(
			"%s:%s was removed and connected again while the call started; repeat the command", acct.Provider, acct.Alias)}
	case errors.Is(err, proxy.ErrNoCredential):
		return notConnected(accountRef{provider: acct.Provider, alias: acct.Alias})
	case errors.Is(err, proxy.ErrNoOAuthClient):
		return &Error{Code: "no-oauth-client", Exit: ExitUsage, Message: fmt.Sprintf(
			"no OAuth client for %s; run `addition client add %s --file client_secret.json`", acct.Provider, provider.IdentityRoot)}
	case errors.As(err, &refused):
		return &Error{Code: "refresh-rejected", Message: refused.Error(), Exit: upstreamExit(refused.Status)}
	case errors.As(err, &tokenNet), errors.As(err, &apiNet):
		return &Error{Code: "network", Message: err.Error(), Exit: ExitUpstreamServer}
	}
	if reqErr, ok := errors.AsType[*proxy.RequestError](err); ok {
		return callRequestError(reqErr)
	}
	return vaultError(err)
}

func upstreamExit(status int) int {
	if status >= 500 {
		return ExitUpstreamServer
	}
	return ExitUpstreamClient
}

// app resolves the app a command names. An identity root is refused: as a legacy-account error
// when the vault still holds its account, else as not-an-app.
func (a *accountSession) app(ctx context.Context, name, alias string) (*catalog.Provider, error) {
	p, ok := lookupProvider(a.catalog, name)
	if !ok || a.catalog.Connectable(p.ID) {
		return appProvider(a.catalog, name)
	}
	stored, err := a.store.ListAccounts(ctx, p.ID)
	if err != nil {
		return nil, vaultError(err)
	}
	for _, acct := range stored {
		if alias == "" || acct.Alias == alias {
			return nil, legacyAccountError(a.catalog, acct)
		}
	}
	return nil, notAnApp(a.catalog, p)
}

// callAccount picks the app's account a call uses: the alias, else the app's default, else its only account.
func (a *accountSession) callAccount(ctx context.Context, app *catalog.Provider, alias string) (vault.Account, error) {
	if alias != "" {
		acct, err := a.store.GetAccount(ctx, app.ID, alias)
		if errors.Is(err, vault.ErrNotFound) {
			return vault.Account{}, notConnected(accountRef{provider: app.ID, alias: alias})
		}
		if err != nil {
			return vault.Account{}, vaultError(err)
		}
		return acct, nil
	}
	candidates, err := a.store.ListAccounts(ctx, app.ID)
	if err != nil {
		return vault.Account{}, vaultError(err)
	}
	if len(candidates) == 0 {
		return vault.Account{}, &Error{Code: "not-connected", Exit: ExitNeedsConnect,
			Message: fmt.Sprintf("no %s account is connected; run `addition connect %s --as <alias>`", app.ID, app.ID)}
	}
	if def, err := a.store.DefaultAccount(ctx, app.ID); err == nil {
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
		"several %s accounts are connected (%s); pass -a <alias> or set a default with `addition accounts default %s:<alias>`",
		app.ID, strings.Join(aliases, ", "), app.ID)}
}

func readRequestBody(ctx context.Context, stdin io.Reader, f callFlags) ([]byte, error) {
	switch {
	case !f.dataSet:
		return nil, nil
	case f.data == "-":
		return readCapped(ctx, stdin)
	case strings.HasPrefix(f.data, "@"):
		return readBodyFile(ctx, f.data[1:])
	}
	return capBody([]byte(f.data))
}

func readBodyFile(ctx context.Context, path string) ([]byte, error) {
	if path == "" {
		return nil, &Error{Code: "bad-data", Exit: ExitUsage, Message: "--data @ needs a file name"}
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, &Error{Code: "bad-data", Exit: ExitUsage, Message: fmt.Sprintf("read the --data file: %v", err)}
	}
	defer file.Close()
	return readCapped(ctx, file)
}

// readCapped reads r in the background so cancelling ctx ends the wait; the read itself ends with the process.
func readCapped(ctx context.Context, r io.Reader) ([]byte, error) {
	type read struct {
		data []byte
		err  error
	}
	done := make(chan read, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(r, maxRequestBody+1))
		done <- read{data, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			return nil, &Error{Code: "bad-data", Exit: ExitUsage, Message: fmt.Sprintf("read the request body: %v", got.err)}
		}
		return capBody(got.data)
	case <-ctx.Done():
		return nil, interrupted()
	}
}

func capBody(data []byte) ([]byte, error) {
	if int64(len(data)) > maxRequestBody {
		return nil, &Error{Code: "body-too-large", Exit: ExitUsage, Message: fmt.Sprintf("the request body is over %d bytes", maxRequestBody)}
	}
	return data, nil
}

// deliver writes the final response: a success body to stdout or the -o file, anything else as an upstream failure.
// Everything derived from the response passes the echo guard, so an access token never leaves addition.
func deliver(cmd *cobra.Command, method string, resp *proxy.Response, out *outputFile) error {
	guard := proxy.NewEchoGuard(resp.Tokens)
	if mayHaveBody(method, resp.Status) && hasContentCoding(resp.Header) {
		return unsupportedEncoding(resp.Status)
	}
	if resp.Status < 200 || resp.Status > 299 {
		return deliverFailure(cmd, resp, guard)
	}
	if out != nil {
		return out.save(cmd, resp, guard)
	}
	body := bufio.NewReaderSize(guard.Reader(resp.Body), sniffLength)
	head, err := body.Peek(sniffLength)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return readFailure(cmd, err)
	}
	if len(head) > 0 && !isTextual(resp.Header.Get("Content-Type"), head) {
		return binaryResponse()
	}
	return copyBody(cmd, cmd.OutOrStdout(), body)
}

// deliverFailure prints the upstream body, capped, and reports only the status. A redirect's body is never read.
func deliverFailure(cmd *cobra.Command, resp *proxy.Response, guard *proxy.EchoGuard) error {
	if resp.Status < 300 || resp.Status > 399 {
		inspected := io.LimitReader(resp.Body, maxErrorBody+int64(guard.Lookahead()))
		if err := copyBody(cmd, cmd.OutOrStdout(), io.LimitReader(guard.Reader(inspected), maxErrorBody)); err != nil {
			return err
		}
	}
	return &Error{Code: "upstream-error", Message: fmt.Sprintf("HTTP %d", resp.Status), Exit: upstreamExit(resp.Status)}
}

// bodyIsRead is false for a redirect, whose body is never read.
func bodyIsRead(status int) bool { return status < 300 || status > 399 }

// mayHaveBody is false where the response carries no body to inspect: HEAD, 204, 304 and redirects.
func mayHaveBody(method string, status int) bool {
	return method != http.MethodHead && status != http.StatusNoContent && status != http.StatusNotModified && bodyIsRead(status)
}

// hasContentCoding reports any coding on the body; the transport never decodes one.
func hasContentCoding(h http.Header) bool {
	for _, value := range h.Values("Content-Encoding") {
		for coding := range strings.SplitSeq(value, ",") {
			if coding = strings.ToLower(strings.TrimSpace(coding)); coding != "" && coding != "identity" {
				return true
			}
		}
	}
	return false
}

func unsupportedEncoding(status int) *Error {
	exit := ExitUpstreamServer
	if status > 299 {
		exit = upstreamExit(status)
	}
	return &Error{Code: "unsupported-encoding", Exit: exit,
		Message: fmt.Sprintf("the HTTP %d response has a content coding addition cannot inspect for the access token; it was withheld", status)}
}

func binaryResponse() *Error {
	return &Error{Code: "binary-response", Exit: ExitUsage,
		Message: "the response is not text or JSON; repeat the call with -o <file> to save it"}
}

func networkFailure() *Error {
	return &Error{Code: "network", Message: (&proxy.NetworkError{}).Error(), Exit: ExitUpstreamServer}
}

// readFailure says why a response body stopped: it repeated a token, the user interrupted the command, or the connection broke.
func readFailure(cmd *cobra.Command, err error) *Error {
	switch {
	case errors.Is(err, proxy.ErrCredentialEcho):
		return credentialEcho()
	case cmd.Context().Err() != nil || errors.Is(err, context.Canceled):
		return interrupted()
	}
	return networkFailure()
}

func interrupted() *Error {
	return &Error{Code: "interrupted", Exit: ExitInternal, Message: "interrupted before the command finished"}
}

func credentialEcho() *Error {
	return &Error{Code: "credential-echo", Exit: ExitUpstreamServer,
		Message: "the API response repeated the access token and was withheld"}
}

// copyBody streams src to dst, telling a broken upstream from a broken stdout.
func copyBody(cmd *cobra.Command, dst io.Writer, src io.Reader) error {
	watched := &failureReader{r: src}
	_, err := io.Copy(dst, watched)
	if watched.err != nil {
		return readFailure(cmd, watched.err)
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
