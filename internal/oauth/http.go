package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/runagent-dev/addition/internal/catalog"
)

// Tokens is a parsed token response. Scopes is nil when the provider did not say what it granted.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresIn    time.Duration
	Scopes       []string
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

	const op = "token exchange"
	status, body, err := send(hc, req, op)
	if err != nil {
		return Tokens{}, err
	}
	parsed := parseBody(body, cfg.TokenResponseFormat)
	if adapter.isError(status, parsed) {
		code, description := adapter.errorDetail(parsed)
		return Tokens{}, &ProviderError{Op: op, Status: status, Code: code, Description: description}
	}
	return extractTokens(cfg, parsed, status)
}

func send(hc *http.Client, req *http.Request, op string) (int, []byte, error) {
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, &NetworkError{Op: op, Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, &NetworkError{Op: op, Err: err}
	}
	return resp.StatusCode, body, nil
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
	access, _ := lookup(body, paths.AccessToken).(string)
	if access == "" {
		return Tokens{}, &ProviderError{Op: "token exchange", Status: status, Code: "malformed_token_response",
			Description: "no access token at '" + paths.AccessToken + "'"}
	}
	t := Tokens{AccessToken: access}
	t.RefreshToken, _ = lookup(body, paths.RefreshToken).(string)
	t.TokenType, _ = lookup(body, "token_type").(string)
	if seconds, ok := asSeconds(lookup(body, paths.ExpiresIn)); ok {
		t.ExpiresIn = time.Duration(seconds) * time.Second
	}
	if scope, _ := lookup(body, "scope").(string); scope != "" {
		t.Scopes = splitScopes(cfg, scope)
	}
	return t, nil
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
// A provider without one has no identity, reported as "".
func FetchIdentity(ctx context.Context, hc *http.Client, p *catalog.Provider, accessToken string) (string, error) {
	id := p.OAuth2.Identity
	if id == nil {
		return "", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id.URL, nil)
	if err != nil {
		return "", err
	}
	header, prefix := p.API.AuthHeader, p.API.AuthPrefix
	if header == "" {
		header, prefix = "Authorization", "Bearer"
	}
	req.Header.Set(header, strings.TrimSpace(prefix+" "+accessToken))
	req.Header.Set("Accept", "application/json")

	const op = "identity lookup"
	status, body, err := send(hc, req, op)
	if err != nil {
		return "", err
	}
	parsed := parseBody(body, "json")
	if status >= 400 {
		code, description := defaultAdapter{}.errorDetail(parsed)
		return "", &ProviderError{Op: op, Status: status, Code: code, Description: description}
	}
	identity, _ := lookup(parsed, id.Field).(string)
	if identity == "" {
		return "", &ProviderError{Op: op, Status: status, Code: "malformed_identity_response", Description: "no string at '" + id.Field + "'"}
	}
	return identity, nil
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

	const op = "revoke"
	status, body, err := send(hc, req, op)
	if err != nil {
		return err
	}
	if status >= 400 {
		code, description := defaultAdapter{}.errorDetail(parseBody(body, "json"))
		return &ProviderError{Op: op, Status: status, Code: code, Description: description}
	}
	return nil
}
