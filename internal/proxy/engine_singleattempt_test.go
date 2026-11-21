package proxy_test

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/runagent-dev/addition/internal/proxy"
)

// refusingFirstStreamServer is a TLS server that speaks just enough HTTP/2 to refuse the first
// stream (REFUSED_STREAM) and answer later ones, and that closes an HTTP/1.1 request unanswered.
// requests counts HTTP/2 streams and HTTP/1.1 requests alike.
type refusingFirstStreamServer struct {
	srv      *httptest.Server
	requests atomic.Int32
	h2Conns  atomic.Int32
}

func newRefusingFirstStreamServer(t *testing.T) *refusingFirstStreamServer {
	t.Helper()
	s := &refusingFirstStreamServer{}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.requests.Add(1)
		closeWithoutAnswering(w)
	}))
	s.srv.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	s.srv.Config.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){
		"h2": func(_ *http.Server, conn *tls.Conn, _ http.Handler) { s.serveH2(conn) },
	}
	s.srv.StartTLS()
	t.Cleanup(s.srv.Close)
	listTestHost(s.srv.URL)
	return s
}

const (
	frameHeaders, frameRstStream, frameSettings = 1, 3, 4
	flagAck, flagEndStream, flagEndHeaders      = 1, 1, 4
	errRefusedStream                            = 7
	hpackStatus200                              = 0x88
)

func writeFrame(w io.Writer, kind, flags byte, stream uint32, payload ...byte) {
	head := []byte{0, 0, byte(len(payload)), kind, flags, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(head[5:], stream)
	_, _ = w.Write(append(head, payload...))
}

func (s *refusingFirstStreamServer) serveH2(conn *tls.Conn) {
	defer conn.Close()
	s.h2Conns.Add(1)
	if _, err := io.ReadFull(conn, make([]byte, len("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"))); err != nil {
		return
	}
	writeFrame(conn, frameSettings, 0, 0)
	head := make([]byte, 9)
	for {
		if _, err := io.ReadFull(conn, head); err != nil {
			return
		}
		length := int(head[0])<<16 | int(head[1])<<8 | int(head[2])
		if _, err := io.CopyN(io.Discard, conn, int64(length)); err != nil {
			return
		}
		stream := binary.BigEndian.Uint32(head[5:]) & 0x7fffffff
		switch {
		case head[3] == frameSettings && head[4]&flagAck == 0:
			writeFrame(conn, frameSettings, flagAck, 0)
		case head[3] == frameHeaders && s.requests.Add(1) == 1:
			writeFrame(conn, frameRstStream, 0, stream, 0, 0, 0, errRefusedStream)
		case head[3] == frameHeaders:
			writeFrame(conn, frameHeaders, flagEndHeaders|flagEndStream, stream, hpackStatus200)
		}
	}
}

func (s *refusingFirstStreamServer) http2Client() *http.Client {
	base := s.srv.Client().Transport.(*http.Transport).Clone()
	base.ForceAttemptHTTP2 = true
	return &http.Client{Transport: base}
}

func TestEngineNeverRetriesAStreamTheServerRefused(t *testing.T) {
	for _, method := range []string{"DELETE", "POST", "GET"} {
		t.Run(method, func(t *testing.T) {
			server := newRefusingFirstStreamServer(t)
			e := newEngine(&fakeRenewer{}, &sleeps{})
			e.Client = server.http2Client()
			target, _ := url.Parse(server.srv.URL + "/x")

			_, err := e.Do(context.Background(), proxy.Request{Method: method, URL: target, Header: http.Header{"Idempotency-Key": {"k"}}}, "t")

			var network *proxy.NetworkError
			if !errors.As(err, &network) {
				t.Errorf("err = %v, want a NetworkError", err)
			}
			if n := server.requests.Load(); n != 1 {
				t.Errorf("the server saw %d requests, want exactly 1: a refused stream must not be sent again", n)
			}
		})
	}
}

func TestEngineNeverTakesAnIdleConnectionToReplayABodylessMutation(t *testing.T) {
	up := newUpstream(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		switch n {
		case 1:
			_, _ = w.Write([]byte("primed"))
		case 2:
			closeWithoutAnswering(w)
		default:
			_, _ = w.Write([]byte("replayed"))
		}
	})
	base := &http.Transport{}
	t.Cleanup(base.CloseIdleConnections)
	primer, err := (&http.Client{Transport: base}).Get(up.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, primer.Body)
	_ = primer.Body.Close()
	e := newEngine(&fakeRenewer{}, &sleeps{})
	e.Client = &http.Client{Transport: base}

	_, err = e.Do(context.Background(), proxy.Request{Method: "DELETE", URL: up.url("/x"), Header: http.Header{"Idempotency-Key": {"k"}}}, "t")

	var network *proxy.NetworkError
	if !errors.As(err, &network) {
		t.Errorf("err = %v, want a NetworkError", err)
	}
	if n := len(up.seen()); n != 2 {
		t.Errorf("the server saw %d requests, want 2: the priming GET and one DELETE, never a replay", n)
	}
}

func TestEngineLeavesTheCallersTransportAsItWas(t *testing.T) {
	up := newUpstream(t, reply(200, "ok"))
	base := &http.Transport{ForceAttemptHTTP2: true}
	e := newEngine(&fakeRenewer{}, &sleeps{})
	e.Client = &http.Client{Transport: base}

	if _, _, err := do(t, e, proxy.Request{Method: "GET", URL: up.url("/")}, "t"); err != nil {
		t.Fatal(err)
	}

	if base.DisableKeepAlives || !base.ForceAttemptHTTP2 || base.Protocols != nil {
		t.Error("the caller's transport was changed; the token refresh shares it")
	}
}
