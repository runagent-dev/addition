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
	ErrBadRedirect   = errors.New("not an OAuth redirect: expected the full URL the browser landed on, with code and state")
	ErrStateMismatch = errors.New("the redirect's state does not match this authorization")
)

// AuthorizationError is an error the provider reported in the redirect, such as access_denied.
type AuthorizationError struct {
	Code        string
	Description string
}

func (e *AuthorizationError) Error() string {
	return describe("authorization failed", e.Code, e.Description)
}

func describe(prefix, code, description string) string {
	msg := prefix + ": " + code
	if description != "" {
		msg += " (" + description + ")"
	}
	return msg
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
	RedirectURI      string
	Code             string
	State            string
	Error            string
	ErrorDescription string
}

// ParseCallback reads a redirect URL the browser landed on.
func ParseCallback(raw string) (Callback, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Callback{}, ErrBadRedirect
	}
	cb := callbackFromQuery(u.Query())
	if cb.State == "" || (cb.Code == "" && cb.Error == "") {
		return Callback{}, ErrBadRedirect
	}
	cb.RedirectURI = (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
	return cb, nil
}

func callbackFromQuery(q url.Values) Callback {
	return Callback{
		Code: q.Get("code"), State: q.Get("state"),
		Error: q.Get("error"), ErrorDescription: q.Get("error_description"),
	}
}

// Verify checks the callback belongs to the authorization with state and returns its code.
func (cb Callback) Verify(state string) (string, error) {
	if !constantTimeEqual(cb.State, state) {
		return "", ErrStateMismatch
	}
	if cb.Error != "" {
		return "", &AuthorizationError{Code: cb.Error, Description: cb.ErrorDescription}
	}
	if cb.Code == "" {
		return "", ErrBadRedirect
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

// ProviderError is a provider refusing a request: an error status or an unusable body.
type ProviderError struct {
	Op          string
	Status      int
	Code        string
	Description string
}

func (e *ProviderError) Error() string {
	return describe(fmt.Sprintf("%s rejected (HTTP %d)", e.Op, e.Status), e.Code, e.Description)
}

// NetworkError is a request that never got an HTTP response.
type NetworkError struct {
	Op  string
	Err error
}

func (e *NetworkError) Error() string { return e.Op + ": " + e.Err.Error() }

func (e *NetworkError) Unwrap() error { return e.Err }
