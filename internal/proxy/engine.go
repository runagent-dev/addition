package proxy

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/runagent-dev/addition/internal/catalog"
)

const (
	maxThrottleRetries = 3
	maxRetryWait       = 30 * time.Second
	baseBackoff        = time.Second
	drainLimit         = 64 << 10
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
}

// Do sends req with accessToken. A 401 is renewed and retried once; a 429 is retried up to
// three times, waiting for Retry-After or backing off. Any other response, including a second
// 401 or an exhausted 429, is returned as is.
func (e *Engine) Do(ctx context.Context, req Request, accessToken string) (*Response, error) {
	renewed, throttled := false, 0
	var sent []string
	for {
		if !slices.Contains(sent, accessToken) {
			sent = append(sent, accessToken)
		}
		resp, err := e.send(ctx, req, accessToken)
		if err != nil {
			return nil, err
		}
		resp.Tokens = sent
		switch {
		case resp.Status == http.StatusUnauthorized && !renewed:
			renewed = true
			discard(resp)
			if accessToken, err = e.renew(ctx, accessToken); err != nil {
				return nil, &AuthFailure{Status: http.StatusUnauthorized, Err: err}
			}
		case resp.Status == http.StatusTooManyRequests && throttled < maxThrottleRetries:
			wait := e.retryDelay(resp.Header, throttled)
			discard(resp)
			throttled++
			if err := e.sleep(ctx, wait); err != nil {
				return nil, err
			}
		default:
			return resp, nil
		}
	}
}

func (e *Engine) renew(ctx context.Context, rejected string) (string, error) {
	if e.Renewer == nil {
		return "", ErrNoRefreshToken
	}
	return e.Renewer.Renew(ctx, rejected)
}

func (e *Engine) send(ctx context.Context, req Request, accessToken string) (*Response, error) {
	attempt, cancel := context.WithCancel(ctx)
	headersTimer := e.startHeaderTimer(cancel)
	httpReq, err := e.build(attempt, req, accessToken)
	if err != nil {
		cancel()
		return nil, &RequestError{Code: "bad-request", Message: "the request could not be built"}
	}
	client := &http.Client{
		Transport:     e.Client.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(httpReq)
	if headersTimer != nil {
		headersTimer.Stop()
	}
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &NetworkError{err: err}
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}}, nil
}

func (e *Engine) startHeaderTimer(cancel context.CancelFunc) *time.Timer {
	if e.Client.Timeout <= 0 {
		return nil
	}
	return time.AfterFunc(e.Client.Timeout, cancel)
}

func (e *Engine) build(ctx context.Context, req Request, accessToken string) (*http.Request, error) {
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, req.URL.String(), body)
	if err != nil {
		return nil, err
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
	if seconds, err := strconv.ParseUint(value, 10, 32); err == nil {
		return time.Duration(seconds) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(e.now()), 0), true
	}
	return 0, false
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

// discard closes a response that will be retried, reading only a little of its body.
func discard(resp *Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	_ = resp.Body.Close()
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
