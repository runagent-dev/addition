// Package faultpeer is a local TCP peer for tests that breaks HTTP requests at chosen points.
package faultpeer

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
)

// Fault is where the peer resets the connection.
type Fault int

const (
	// ResetOnFirstByte resets once the request starts arriving, so the client already holds the connection.
	ResetOnFirstByte Fault = iota
	// ResetMidRequest resets after reading the first PartialBytes of the request.
	ResetMidRequest
	// ResetAfterRequest resets after reading the whole request, body included.
	ResetAfterRequest
)

// PartialBytes is how much of a request ResetMidRequest reads; send a larger body than this.
const PartialBytes = 2048

// Listen starts a peer on 127.0.0.1 and returns its http:// base URL.
func Listen(t *testing.T, fault Fault) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go breakConn(conn.(*net.TCPConn), fault)
		}
	}()
	return "http://" + ln.Addr().String()
}

func breakConn(conn *net.TCPConn, fault Fault) {
	switch fault {
	case ResetOnFirstByte:
		_, _ = io.ReadFull(conn, make([]byte, 1))
	case ResetMidRequest:
		_, _ = io.ReadFull(conn, make([]byte, PartialBytes))
	case ResetAfterRequest:
		if req, err := http.ReadRequest(bufio.NewReader(conn)); err == nil {
			_, _ = io.Copy(io.Discard, req.Body)
		}
	}
	_ = conn.SetLinger(0)
	_ = conn.Close()
}
