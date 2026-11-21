// Package proxy sends authenticated requests to a provider's API: it validates where a request
// may go, injects the stored access token, refreshes it under the vault lock, and retries
// throttling and one expired token. It never follows redirects.
package proxy

import "errors"

var (
	ErrNeedsReauth    = errors.New("the account needs to be reconnected")
	ErrNoRefreshToken = errors.New("the account has no refresh token")
	ErrNoCredential   = errors.New("the account has no stored credential")
	ErrNoOAuthClient  = errors.New("no OAuth client is stored for the provider")
)

// RequestError is a request addn refuses to send. Code is a stable machine-readable identifier
// and Message never repeats the caller's path, query, header values or URL userinfo.
type RequestError struct {
	Code    string
	Message string
}

func (e *RequestError) Error() string { return e.Message }

// NetworkError is a request that got no HTTP response. Its text never names the URL or any header.
type NetworkError struct{ err error }

func (e *NetworkError) Error() string { return "network error contacting the provider's API" }

func (e *NetworkError) Unwrap() error { return e.err }

// AuthFailure is an access token the API rejected that could not be renewed. Err says why.
type AuthFailure struct {
	Status int
	Err    error
}

func (e *AuthFailure) Error() string {
	return "the provider rejected the access token and it could not be renewed: " + e.Err.Error()
}

func (e *AuthFailure) Unwrap() error { return e.Err }

// Attempted wraps a failure that came after a request was sent. Status is the last status
// the API answered with, 0 if it never answered.
type Attempted struct {
	Status int
	Err    error
}

func (e *Attempted) Error() string { return e.Err.Error() }

func (e *Attempted) Unwrap() error { return e.Err }
