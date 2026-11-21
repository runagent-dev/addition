package cli

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// signalingReader reports its first Read and then blocks until closed.
type signalingReader struct {
	reading chan struct{}
	closed  chan struct{}
}

func newSignalingReader() *signalingReader {
	return &signalingReader{reading: make(chan struct{}), closed: make(chan struct{})}
}

func (r *signalingReader) Read([]byte) (int, error) {
	select {
	case <-r.reading:
	default:
		close(r.reading)
	}
	<-r.closed
	return 0, io.EOF
}

func TestCallStopsReadingStdinOnTheFirstCancel(t *testing.T) {
	c := newCallHarness(t)
	stdin := newSignalingReader()
	t.Cleanup(func() { close(stdin.closed) })
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan result, 1)
	go func() { finished <- c.runContext(ctx, stdin, "call", "-a", "work", "POST", "/x", "--data", "-") }()

	<-stdin.reading
	cancel()

	select {
	case r := <-finished:
		c.wantError(r, ExitInternal, "interrupted")
	case <-time.After(5 * time.Second):
		t.Fatal("the call is still blocked reading stdin after it was cancelled")
	}
	if n := len(c.api.requests()); n != 0 {
		t.Errorf("the API saw %d requests, want none", n)
	}
}

func TestCallReportsACancelledErrorBodyAsInterrupted(t *testing.T) {
	c := newCallHarness(t)
	stalled, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	c.api.respond(func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		close(stalled)
		<-release
	})
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan result, 1)
	go func() { finished <- c.runContext(ctx, nil, "call", "-a", "work", "GET", "/x") }()

	<-stalled
	cancel()

	select {
	case r := <-finished:
		c.wantError(r, ExitInternal, "interrupted")
	case <-time.After(5 * time.Second):
		t.Fatal("the call is still blocked reading the error body after it was cancelled")
	}
}
