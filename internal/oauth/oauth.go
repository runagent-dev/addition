// Package oauth runs the OAuth 2.0 authorization-code flow from catalog data:
// authorize URL, PKCE, state, code exchange, identity lookup and revocation.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/runagent-dev/addition/internal/catalog"
)

// Client is the user's own OAuth client for a provider.
type Client struct {
	ID     string
	Secret string
}

// Authorization is one authorization request in flight. Verifier is empty without PKCE.
type Authorization struct {
	State       string
	Verifier    string
	RedirectURI string
	Scopes      []string
}

var (
	ErrBadRedirect   = errors.New("not an OAuth redirect: expected the full URL the browser landed on, with state and either code or error")
	ErrStateMismatch = errors.New("the redirect's state does not match this authorization")
)

// AuthorizationError is the provider refusing consent in the redirect. Code is an allowlisted
// OAuth error identifier, or empty; the provider's own text is never kept.
type AuthorizationError struct {
	Code string
}

func (e *AuthorizationError) Error() string {
	return withCode("the provider refused the authorization", e.Code)
}

func withCode(msg, code string) string {
	if code != "" {
		msg += ": " + code
	}
	return msg
}

// knownErrorCodes are the RFC 6749 and RFC 6750 error identifiers addn repeats to the user.
var knownErrorCodes = map[string]bool{
	"invalid_request": true, "invalid_client": true, "invalid_grant": true, "unauthorized_client": true,
	"unsupported_grant_type": true, "invalid_scope": true, "access_denied": true, "server_error": true,
	"temporarily_unavailable": true, "invalid_token": true,
}

// allowlisted returns code when it is a known OAuth error identifier, else "".
func allowlisted(code string) string {
	if knownErrorCodes[code] {
		return code
	}
	return ""
}

// NewAuthorization starts a request with a random state and, when the provider supports it, a PKCE verifier.
func NewAuthorization(cfg *catalog.OAuth2, redirectURI string, scopes []string) (Authorization, error) {
	state, err := randomToken()
	if err != nil {
		return Authorization{}, err
	}
	a := Authorization{State: state, RedirectURI: redirectURI, Scopes: scopes}
	if cfg.PKCESupported || cfg.PKCERequired {
		if a.Verifier, err = randomToken(); err != nil {
			return Authorization{}, err
		}
	}
	return a, nil
}

// randomToken is 32 random bytes, base64url: 43 characters, a valid RFC 7636 verifier.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Challenge is the S256 PKCE challenge for verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// URL is the provider's authorize URL for a, with the catalog's authorize_params.
func (a Authorization) URL(cfg *catalog.OAuth2, client Client) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", client.ID)
	q.Set("redirect_uri", a.RedirectURI)
	q.Set("state", a.State)
	q.Set("scope", JoinScopes(cfg, a.Scopes))
	if a.Verifier != "" {
		q.Set("code_challenge", Challenge(a.Verifier))
		q.Set("code_challenge_method", "S256")
	}
	for k, v := range cfg.AuthorizeParams {
		q.Set(k, v)
	}
	sep := "?"
	if strings.Contains(cfg.AuthorizeURL, "?") {
		sep = "&"
	}
	return cfg.AuthorizeURL + sep + q.Encode()
}

func scopeDelimiter(cfg *catalog.OAuth2) string {
	if cfg.ScopesDelimiter == "comma" {
		return ","
	}
	return " "
}

func JoinScopes(cfg *catalog.OAuth2, scopes []string) string {
	return strings.Join(scopes, scopeDelimiter(cfg))
}

func splitScopes(cfg *catalog.OAuth2, s string) []string {
	var out []string
	for _, scope := range strings.Split(s, scopeDelimiter(cfg)) {
		if scope = strings.TrimSpace(scope); scope != "" {
			out = append(out, scope)
		}
	}
	return out
}

// Callback is what the provider sent back to the redirect URI.
type Callback struct {
	RedirectURI string
	Code        string
	State       string
	Error       string
	ambiguous   bool
}

// ParseCallback reads a redirect URL the browser landed on.
func ParseCallback(raw string) (Callback, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Callback{}, ErrBadRedirect
	}
	cb := callbackFromQuery(u.Query())
	if cb.State == "" || !cb.wellFormed() {
		return Callback{}, ErrBadRedirect
	}
	cb.RedirectURI = (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
	return cb, nil
}

// callbackFromQuery marks a callback ambiguous when a key repeats or both code and error are present,
// even empty, since q.Get would otherwise pick one value and hide the rest.
func callbackFromQuery(q url.Values) Callback {
	_, hasCode := q["code"]
	_, hasError := q["error"]
	repeated := len(q["code"]) > 1 || len(q["state"]) > 1 || len(q["error"]) > 1
	return Callback{
		Code: q.Get("code"), State: q.Get("state"), Error: q.Get("error"),
		ambiguous: repeated || (hasCode && hasError),
	}
}

// check is Verify without handing out the code: nil for a code or a provider refusal meant for state.
func (cb Callback) check(state string) error {
	if !constantTimeEqual(cb.State, state) {
		return ErrStateMismatch
	}
	if !cb.wellFormed() {
		return ErrBadRedirect
	}
	return nil
}

// wellFormed is a callback carrying exactly one of a code or a provider error.
func (cb Callback) wellFormed() bool { return !cb.ambiguous && (cb.Code == "") != (cb.Error == "") }

// Verify checks the callback belongs to the authorization with state and returns its code.
func (cb Callback) Verify(state string) (string, error) {
	if err := cb.check(state); err != nil {
		return "", err
	}
	if cb.Error != "" {
		return "", &AuthorizationError{Code: allowlisted(cb.Error)}
	}
	return cb.Code, nil
}

func constantTimeEqual(a, b string) bool {
	if a == "" || len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range len(a) {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// ProviderError is a provider refusing a request or answering with something unusable.
// Code is an allowlisted OAuth error identifier or empty; Problem is written by addn.
type ProviderError struct {
	Op      string
	Status  int
	Code    string
	Problem string
}

func (e *ProviderError) Error() string {
	msg := withCode(fmt.Sprintf("%s failed (HTTP %d)", e.Op, e.Status), e.Code)
	if e.Problem != "" {
		msg += ": " + e.Problem
	}
	return msg
}

// NetworkError is a request that got no HTTP response. RequestSent is false only when addn
// never obtained a connection to the provider, so no part of the request was sent.
type NetworkError struct {
	Op          string
	RequestSent bool
	err         error
}

func (e *NetworkError) Error() string { return "network error contacting the " + endpointNames[e.Op] }

func (e *NetworkError) Unwrap() error { return e.err }

const (
	OpExchange = "token exchange"
	OpIdentity = "identity lookup"
	OpRevoke   = "revoke"
)

var endpointNames = map[string]string{
	OpExchange: "token endpoint",
	OpIdentity: "identity endpoint",
	OpRevoke:   "revoke endpoint",
}
