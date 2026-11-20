package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const runMainEnv = "ADDN_TEST_RUN_MAIN"

// TestMain lets a test re-run this binary as the real addn executable.
func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main()
	}
	os.Exit(m.Run())
}

const (
	tokenHost    = "oauth2.googleapis.com"
	secretMarker = "GOCSPX-subprocess-secret-marker"
)

// TestTransportLogsNeverReachStderr runs addn as a process against a token endpoint that answers,
// then sends unsolicited bytes repeating the request, which net/http's transport logs by default.
func TestTransportLogsNeverReachStderr(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("SSL_CERT_FILE, which points addn at the fixture's certificate, is honoured only on Linux")
	}
	dir := privateDir(t)
	proxy, served := tokenEndpointProxy(t, dir)
	env := []string{
		runMainEnv + "=1",
		"HOME=" + privateDir(t),
		"XDG_DATA_HOME=" + privateDir(t),
		"ADDN_HOME=" + filepath.Join(dir, "addn"),
		"HTTPS_PROXY=" + proxy,
		"SSL_CERT_FILE=" + filepath.Join(dir, "ca.pem"),
	}
	addn := func(args ...string) (int, string, string) {
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, stdout.String(), stderr.String()
	}
	mustAddn := func(args ...string) string {
		code, stdout, stderr := addn(args...)
		if code != 0 {
			t.Fatalf("addn %v: exit %d\nstderr: %s", args, code, stderr)
		}
		return stdout
	}

	mustAddn("init", "--key-file")
	mustAddn("client", "add", "google", "--file", clientFile(t, dir))
	var pending struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.Unmarshal([]byte(mustAddn("connect", "google", "--as", "work", "--no-browser", "--services", "gmail")), &pending); err != nil {
		t.Fatal(err)
	}
	authorize, err := url.Parse(pending.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	q := authorize.Query()
	landed := q.Get("redirect_uri") + "?" + url.Values{"code": {"4/fixture-code"}, "state": {q.Get("state")}}.Encode()

	code, _, stderr := addn("connect", "--complete", landed)

	if served.Load() != 1 {
		t.Fatalf("the token endpoint served %d requests, want 1", served.Load())
	}
	if code != 4 {
		t.Errorf("exit = %d, want 4", code)
	}
	if strings.Contains(stderr, secretMarker) {
		t.Error("stderr carries the client secret")
	}
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	for _, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Errorf("stderr line is not JSON: %q", line)
		}
	}
	var envelope struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &envelope); err != nil || envelope.Error.Code != "token-exchange-rejected" {
		t.Errorf("last stderr line = %q, want the token-exchange-rejected envelope", lines[len(lines)-1])
	}
}

func privateDir(t *testing.T) string {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func clientFile(t *testing.T, dir string) string {
	data, _ := json.Marshal(map[string]any{"installed": map[string]any{
		"client_id": "1234-fixture.apps.googleusercontent.com", "client_secret": secretMarker,
		"redirect_uris": []string{"http://localhost"},
	}})
	path := filepath.Join(dir, "client_secret.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// tokenEndpointProxy is an HTTPS proxy that impersonates the token host with a certificate it
// writes to dir/ca.pem. It answers each token request with a 400, then unsolicited bytes that
// repeat the request's form.
func tokenEndpointProxy(t *testing.T, dir string) (string, *atomic.Int32) {
	cert := selfSignedCert(t, filepath.Join(dir, "ca.pem"))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	served := &atomic.Int32{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				connect, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil || connect.Method != http.MethodConnect || connect.Host != tokenHost+":443" {
					return
				}
				_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
				tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
				req, err := http.ReadRequest(bufio.NewReader(tlsConn))
				if err != nil {
					return
				}
				_ = req.ParseForm()
				served.Add(1)
				body := `{"error":"invalid_grant"}`
				_, _ = fmt.Fprintf(tlsConn, "HTTP/1.1 400 Bad Request\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s"+
					"unsolicited %s", len(body), body, req.PostForm.Encode())
				_, _ = io.Copy(io.Discard, tlsConn)
			}()
		}
	}()
	return "http://" + ln.Addr().String(), served
}

func selfSignedCert(t *testing.T, pemPath string) tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: tokenHost},
		DNSNames:              []string{tokenHost},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pemPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
