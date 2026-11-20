package oauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/runagent-dev/addition/internal/catalog"
)

// Tokens is a parsed token response. Scopes is nil when the response has no scope field,
// which RFC 6749 §5.1 means the requested scopes; an empty, non-nil Scopes is a grant of none.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresIn    time.Duration
	Scopes       []string
}

// Identity is who an access token belongs to: a stable Subject and a Display name, such as an email.
type Identity struct {
	Subject string
	Display string
}

var ErrRevokeUnsupported = errors.New("the provider has no revoke endpoint")

const maxResponseBytes = 1 << 20

// Exchange trades an authorization code for tokens at the provider's token endpoint.
func Exchange(ctx context.Context, hc *http.Client, p *catalog.Provider, client Client, a Authorization, code string) (Tokens, error) {
	cfg := p.OAuth2
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {a.RedirectURI},
		"client_id":    {client.ID},
	}
	if cfg.ClientAuthMethod != "header" {
		form.Set("client_secret", client.Secret)
	}
	if a.Verifier != "" {
		form.Set("code_verifier", a.Verifier)
	}
	for k, v := range cfg.TokenParams {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if cfg.ClientAuthMethod == "header" {
		req.SetBasicAuth(url.QueryEscape(client.ID), url.QueryEscape(client.Secret))
	}
	adapter := adapterFor(p.ID)
	adapter.beforeTokenExchange(req)

	status, body, err := send(hc, req, OpExchange)
	if err != nil {
		return Tokens{}, err
	}
	parsed := parseBody(body, cfg.TokenResponseFormat)
	if !isSuccess(status) || adapter.isErrorBody(parsed) {
		return Tokens{}, &ProviderError{Op: OpExchange, Status: status, Code: allowlisted(adapter.errorCode(parsed))}
	}
	return extractTokens(cfg, parsed, status)
}

// send runs one back-channel request on hc's transport, bypassing the client so no redirect is
// ever followed: a request body holding a secret or a token goes only to the URL in the catalog.
func send(hc *http.Client, req *http.Request, op string) (int, []byte, error) {
	transport := hc.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	ctx := req.Context()
	if hc.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, hc.Timeout)
		defer cancel()
	}
	var conn connectionTrace
	req = req.WithContext(httptrace.WithClientTrace(ctx, conn.hooks()))

	resp, err := transport.RoundTrip(req)
	if err != nil {
		return 0, nil, &NetworkError{Op: op, RequestSent: !conn.neverConnected(), err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return resp.StatusCode, nil, nil // a redirect is refused on its status; its body is never needed
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, &NetworkError{Op: op, RequestSent: true, err: err}
	}
	return resp.StatusCode, body, nil
}

// connectionTrace records whether a request was handed a connection, and whether a DNS lookup,
// dial or TLS handshake failed before that.
type connectionTrace struct {
	got, setupFailed atomic.Bool
}

func (c *connectionTrace) hooks() *httptrace.ClientTrace {
	failed := func(err error) {
		if err != nil {
			c.setupFailed.Store(true)
		}
	}
	return &httptrace.ClientTrace{
		GotConn:          func(httptrace.GotConnInfo) { c.got.Store(true) },
		DNSDone:          func(info httptrace.DNSDoneInfo) { failed(info.Err) },
		ConnectDone:      func(_, _ string, err error) { failed(err) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) { failed(err) },
	}
}

// neverConnected is positive evidence that no byte of the request reached the provider.
func (c *connectionTrace) neverConnected() bool {
	return c.setupFailed.Load() && !c.got.Load()
}

// parseBody decodes a JSON or form response; anything unparsable becomes an empty object.
func parseBody(body []byte, format string) map[string]any {
	out := map[string]any{}
	if format == "form" {
		values, err := url.ParseQuery(string(body))
		if err == nil {
			for k := range values {
				out[k] = values.Get(k)
			}
		}
		return out
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	_ = dec.Decode(&out)
	return out
}

func extractTokens(cfg *catalog.OAuth2, body map[string]any, status int) (Tokens, error) {
	paths := catalog.TokenResponseMapping{AccessToken: "access_token", RefreshToken: "refresh_token", ExpiresIn: "expires_in"}
	if m := cfg.TokenResponseMapping; m != nil {
		paths = *m
	}
	malformed := func(problem string) error {
		return &ProviderError{Op: OpExchange, Status: status, Problem: problem}
	}
	access, _ := lookup(body, paths.AccessToken).(string)
	if access == "" {
		return Tokens{}, malformed("the response has no access token at '" + paths.AccessToken + "'")
	}
	t := Tokens{AccessToken: access}
	t.RefreshToken, _ = lookup(body, paths.RefreshToken).(string)
	t.TokenType, _ = lookup(body, "token_type").(string)
	if seconds, ok := asSeconds(lookup(body, paths.ExpiresIn)); ok {
		t.ExpiresIn = time.Duration(seconds) * time.Second
	}
	if raw, present := body["scope"]; present {
		scope, ok := raw.(string)
		if !ok {
			return Tokens{}, malformed("the response's scope is not a string")
		}
		t.Scopes = nonNil(splitScopes(cfg, scope))
	}
	return t, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// lookup follows a dot-path through nested JSON objects.
func lookup(body map[string]any, path string) any {
	var cur any = body
	for _, seg := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[seg]
	}
	return cur
}

func asSeconds(v any) (int64, bool) {
	var s string
	switch x := v.(type) {
	case json.Number:
		s = x.String()
	case string:
		s = x
	default:
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
}

// FetchIdentity reads who the access token belongs to from the provider's identity endpoint.
// A provider without one has no identity, reported as the zero Identity.
func FetchIdentity(ctx context.Context, hc *http.Client, p *catalog.Provider, accessToken string) (Identity, error) {
	id := p.OAuth2.Identity
	if id == nil {
		return Identity{}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id.URL, nil)
	if err != nil {
		return Identity{}, err
	}
	header, prefix := p.API.AuthHeader, p.API.AuthPrefix
	if header == "" {
		header, prefix = "Authorization", "Bearer"
	}
	req.Header.Set(header, strings.TrimSpace(prefix+" "+accessToken))
	req.Header.Set("Accept", "application/json")

	status, body, err := send(hc, req, OpIdentity)
	if err != nil {
		return Identity{}, err
	}
	parsed := parseBody(body, "json")
	if !isSuccess(status) {
		return Identity{}, &ProviderError{Op: OpIdentity, Status: status, Code: allowlisted(defaultAdapter{}.errorCode(parsed))}
	}
	subject, ok := identifier(lookup(parsed, id.SubjectField))
	if !ok {
		return Identity{}, &ProviderError{Op: OpIdentity, Status: status, Problem: "the response has no subject at '" + id.SubjectField + "'"}
	}
	display, _ := lookup(parsed, id.Field).(string)
	if display == "" {
		return Identity{}, &ProviderError{Op: OpIdentity, Status: status, Problem: "the response has no string at '" + id.Field + "'"}
	}
	return Identity{Subject: subject, Display: display}, nil
}

// identifier accepts a non-empty string or a JSON number, which some providers use for account ids.
func identifier(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, x != ""
	case json.Number:
		return x.String(), true
	}
	return "", false
}

// Revoke asks the provider to invalidate token (RFC 7009).
func Revoke(ctx context.Context, hc *http.Client, p *catalog.Provider, token string) error {
	if p.OAuth2.RevokeURL == "" {
		return ErrRevokeUnsupported
	}
	form := url.Values{"token": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.OAuth2.RevokeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	status, body, err := send(hc, req, OpRevoke)
	if err != nil {
		return err
	}
	if !isSuccess(status) {
		return &ProviderError{Op: OpRevoke, Status: status, Code: allowlisted(defaultAdapter{}.errorCode(parseBody(body, "json")))}
	}
	return nil
}
