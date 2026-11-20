package oauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

const callbackPath = "/callback"

const donePage = `<!doctype html><meta charset="utf-8"><title>addn</title>
<p>addn received the authorization response. You can close this tab.</p>`

// Loopback is an RFC 8252 redirect listener on 127.0.0.1 that takes the first callback carrying a state.
type Loopback struct {
	listener  net.Listener
	server    *http.Server
	callbacks chan Callback
}

// ListenLoopback listens on 127.0.0.1:port; port 0 picks a free one.
func ListenLoopback(port int) (*Loopback, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	l := &Loopback{listener: ln, callbacks: make(chan Callback, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+callbackPath, l.handle)
	l.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = l.server.Serve(ln) }()
	return l, nil
}

func (l *Loopback) RedirectURI() string {
	return "http://" + l.listener.Addr().String() + callbackPath
}

func (l *Loopback) handle(w http.ResponseWriter, r *http.Request) {
	cb := callbackFromQuery(r.URL.Query())
	if cb.State == "" {
		http.Error(w, "not an OAuth redirect", http.StatusBadRequest)
		return
	}
	cb.RedirectURI = l.RedirectURI()
	select {
	case l.callbacks <- cb:
	default:
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(donePage))
}

// Wait returns the first callback, or ctx's error when it ends first.
func (l *Loopback) Wait(ctx context.Context) (Callback, error) {
	select {
	case cb := <-l.callbacks:
		return cb, nil
	case <-ctx.Done():
		return Callback{}, ctx.Err()
	}
}

func (l *Loopback) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := l.server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return l.server.Close()
	}
	return nil
}

// FreeLoopbackPort finds a port on 127.0.0.1 that nothing listens on right now.
func FreeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
