package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/runagent-dev/addition/internal/catalog"
)

const (
	maxThrottleRetries = 3
	maxRetryWait       = 30 * time.Second
	baseBackoff        = time.Second
)

// Request is one API request. Body is held in memory so it can be sent again after a retry.
type Request struct {
	Method string
	URL    *url.URL
	Header http.Header
	Body   []byte
}

// Response is the API's final answer; the caller streams Body and closes it.
type Response struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
	// Tokens are the access tokens sent across all attempts, which the response must not repeat.
	Tokens []string
}

// Renewer replaces an access token the API rejected.
type Renewer interface {
	Renew(ctx context.Context, rejected string) (string, error)
}

// Engine sends requests with the account's access token. Client supplies the transport and, as
// its Timeout, how long to wait for response headers; redirects are never followed.
type Engine struct {
	Client  *http.Client
	API     catalog.API
	Renewer Renewer
	Now     func() time.Time
	Sleep   func(ctx context.Context, d time.Duration) error
	// Jitter returns a random duration in [0, max]; nil uses math/rand.
	Jitter func(max time.Duration) time.Duration
	// AllowCleartext lets tests send to http URLs; production sends over https only.
	AllowCleartext bool
	// AfterFunc runs expire after d and returns a stop func; nil uses time.AfterFunc.
	AfterFunc func(d time.Duration, expire func()) (stop func() bool)

	transportOnce sync.Once
	attempts      http.RoundTripper
}

// Do sends req with accessToken. A 401 is renewed and retried once; a 429 is retried up to
// three times, waiting for Retry-After or backing off. Any other response, including a second
// 401 or an exhausted 429, is returned as is. Once a request has been sent, a failure is an *Attempted.
func (e *Engine) Do(ctx context.Context, req Request, accessToken string) (*Response, error) {
	if err := CheckDestination(e.API, req.URL, e.AllowCleartext); err != nil {
		return nil, err
	}
	renewed, throttled, lastStatus := false, 0, 0
	var sent []string
	for {
		if !slices.Contains(sent, accessToken) {
			sent = append(sent, accessToken)
		}
		resp, err := e.send(ctx, req, accessToken)
		if err != nil {
			return nil, afterSending(lastStatus, err)
		}
		lastStatus, resp.Tokens = resp.Status, sent
		switch {
		case resp.Status == http.StatusUnauthorized && !renewed:
			renewed = true
			_ = resp.Body.Close()
			if accessToken, err = e.renew(ctx, accessToken); err != nil {
				return nil, &Attempted{Status: lastStatus, Err: &AuthFailure{Status: lastStatus, Err: err}}
			}
		case resp.Status == http.StatusTooManyRequests && throttled < maxThrottleRetries:
			wait := e.retryDelay(resp.Header, throttled)
			_ = resp.Body.Close()
			throttled++
			if err := e.sleep(ctx, wait); err != nil {
				return nil, &Attempted{Status: lastStatus, Err: err}
			}
		default:
			return resp, nil
		}
	}
}

// afterSending marks err as a failure of a request that was sent, unless it was never built.
func afterSending(lastStatus int, err error) error {
	if _, unsendable := errors.AsType[*RequestError](err); unsendable {
		return err
	}
	return &Attempted{Status: lastStatus, Err: err}
}

func (e *Engine) renew(ctx context.Context, rejected string) (string, error) {
	if e.Renewer == nil {
		return "", ErrNoRefreshToken
	}
	return e.Renewer.Renew(ctx, rejected)
}

// send makes one attempt on the transport itself, so no Location is parsed and no redirect considered.
func (e *Engine) send(ctx context.Context, req Request, accessToken string) (*Response, error) {
	attempt, cancel := context.WithCancel(ctx)
	stopTimer := e.startHeaderTimer(cancel)
	defer stopTimer()
	httpReq, err := e.build(attempt, req, accessToken)
	if err != nil {
		cancel()
		return nil, &RequestError{Code: "bad-request", Message: "the request could not be built"}
	}
	resp, err := e.transport().RoundTrip(httpReq)
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &NetworkError{err: err}
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}}, nil
}

// transport is the client's transport as one attempt per connection: see singleAttempt.
func (e *Engine) transport() http.RoundTripper {
	e.transportOnce.Do(func() {
		var base http.RoundTripper = http.DefaultTransport
		if e.Client != nil && e.Client.Transport != nil {
			base = e.Client.Transport
		}
		e.attempts = singleAttempt(base)
	})
	return e.attempts
}

// singleAttempt returns a copy of an *http.Transport that cannot send a request twice by itself:
// every request gets a fresh HTTP/1.1 connection, so Go's retry of a request on a reused connection
// and HTTP/2's retry of a refused stream both have nothing to act on. The proxy and TLS settings are kept.
// Any other RoundTripper is used as given.
func singleAttempt(base http.RoundTripper) http.RoundTripper {
	original, ok := base.(*http.Transport)
	if !ok {
		return base
	}
	clone := original.Clone()
	clone.DisableKeepAlives = true
	clone.ForceAttemptHTTP2 = false
	clone.Protocols = new(http.Protocols)
	clone.Protocols.SetHTTP1(true)
	clone.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if clone.TLSClientConfig != nil {
		clone.TLSClientConfig.NextProtos = slices.DeleteFunc(slices.Clone(clone.TLSClientConfig.NextProtos), func(proto string) bool { return proto == "h2" })
	}
	return clone
}

// startHeaderTimer cancels the attempt if no response head arrives within the client's timeout.
func (e *Engine) startHeaderTimer(cancel context.CancelFunc) (stop func()) {
	if e.Client == nil || e.Client.Timeout <= 0 {
		return func() {}
	}
	afterFunc := e.AfterFunc
	if afterFunc == nil {
		afterFunc = func(d time.Duration, expire func()) func() bool { return time.AfterFunc(d, expire).Stop }
	}
	stopTimer := afterFunc(e.Client.Timeout, cancel)
	return func() { stopTimer() }
}

// build makes a request with no GetBody that closes its connection, so a transport that is not
// the single-attempt clone still has nothing to replay.
func (e *Engine) build(ctx context.Context, req Request, accessToken string) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL.String(), nil)
	if err != nil {
		return nil, err
	}
	httpReq.Close = true
	if len(req.Body) > 0 {
		httpReq.Body = io.NopCloser(bytes.NewReader(req.Body))
		httpReq.ContentLength = int64(len(req.Body))
	}
	for name, values := range req.Header {
		httpReq.Header[name] = append([]string(nil), values...)
	}
	if len(req.Body) > 0 && httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	header, prefix := e.API.AuthHeader, e.API.AuthPrefix
	if header == "" {
		header, prefix = "Authorization", "Bearer"
	}
	httpReq.Header.Set(header, strings.TrimSpace(prefix+" "+accessToken))
	return httpReq, nil
}

// retryDelay is the wait before a throttled request is sent again: the server's Retry-After if
// it is valid, else capped exponential backoff with jitter. It never exceeds maxRetryWait.
func (e *Engine) retryDelay(h http.Header, retried int) time.Duration {
	if wait, ok := e.retryAfter(h); ok {
		return min(wait, maxRetryWait)
	}
	ceiling := min(baseBackoff<<retried, maxRetryWait)
	return ceiling/2 + e.jitter(ceiling/2)
}

func (e *Engine) retryAfter(h http.Header) (time.Duration, bool) {
	name := "Retry-After"
	if e.API.RateLimit != nil && e.API.RateLimit.RetryAfterHeader != "" {
		name = e.API.RateLimit.RetryAfterHeader
	}
	value := strings.TrimSpace(h.Get(name))
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) && isAllDigits(value) {
		return time.Duration(min(seconds, uint64(maxRetryWait/time.Second))) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(e.now()), 0), true
	}
	return 0, false
}

func isAllDigits(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(r rune) bool { return r < '0' || r > '9' })
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) jitter(upTo time.Duration) time.Duration {
	if e.Jitter != nil {
		return e.Jitter(upTo)
	}
	return time.Duration(rand.Int64N(int64(upTo) + 1))
}

func (e *Engine) sleep(ctx context.Context, d time.Duration) error {
	if e.Sleep != nil {
		return e.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cancelOnClose ends the request's context when its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
