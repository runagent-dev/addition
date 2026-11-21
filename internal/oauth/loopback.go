package oauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

const callbackPath = "/callback"

const donePage = `<!doctype html><meta charset="utf-8"><title>addition</title>
<p>addition received the authorization response. You can close this tab.</p>`

// Loopback is an RFC 8252 redirect listener on 127.0.0.1. It answers 400 to any callback that
// is not for its state, and delivers the first verified one: a code or a provider refusal.
type Loopback struct {
	listener  net.Listener
	server    *http.Server
	state     string
	callbacks chan Callback

	mu            sync.Mutex
	accepted      bool
	lastRejection error
}

// ListenLoopback listens on 127.0.0.1:port for the callback of the authorization with state; port 0 picks a free one.
func ListenLoopback(port int, state string) (*Loopback, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	l := &Loopback{listener: ln, state: state, callbacks: make(chan Callback, 1)}
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
	cb := callbackFromQuery(r.URL.RawQuery)
	cb.RedirectURI = l.RedirectURI()
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := cb.check(l.state); err != nil {
		l.lastRejection = err
		http.Error(w, "not the OAuth redirect addition is waiting for", http.StatusBadRequest)
		return
	}
	if l.accepted {
		http.Error(w, "addition already received the authorization response", http.StatusConflict)
		return
	}
	l.accepted = true
	l.callbacks <- cb
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(donePage))
}

// Received yields the one verified callback.
func (l *Loopback) Received() <-chan Callback { return l.callbacks }

// LastRejection is why the most recent invalid callback was refused, or nil if none was.
func (l *Loopback) LastRejection() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastRejection
}

// Close stops listening; it is safe to call more than once.
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
