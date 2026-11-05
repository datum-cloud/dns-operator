// SPDX-License-Identifier: AGPL-3.0-only

package transport

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// authBroker speaks just enough of the NATS protocol to accept or refuse a
// client's password and, like a broker reloading new credentials, to drop
// every connected client with an authorization error.
type authBroker struct {
	ln       net.Listener
	mu       sync.Mutex
	password string
	conns    map[net.Conn]bool
	accepted int
}

func newAuthBroker(t *testing.T, password string) *authBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &authBroker{ln: ln, password: password, conns: map[net.Conn]bool{}}
	t.Cleanup(func() {
		_ = ln.Close()
		b.mu.Lock()
		defer b.mu.Unlock()
		for c := range b.conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go b.serve(c)
		}
	}()
	return b
}

func (b *authBroker) url() string { return "nats://" + b.ln.Addr().String() }

func (b *authBroker) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	if _, err := fmt.Fprintf(c, "INFO {\"server_id\":\"test\",\"version\":\"2.14.0\",\"proto\":1,\"headers\":true,\"auth_required\":true,\"max_payload\":1048576}\r\n"); err != nil {
		return
	}
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		switch {
		case strings.HasPrefix(line, "CONNECT "):
			var connect struct {
				Pass string `json:"pass"`
			}
			_ = json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(line), "CONNECT ")), &connect)
			b.mu.Lock()
			ok := connect.Pass == b.password
			if ok {
				b.conns[c] = true
				b.accepted++
			}
			b.mu.Unlock()
			if !ok {
				_, _ = fmt.Fprint(c, "-ERR 'Authorization Violation'\r\n")
				return
			}
		case strings.HasPrefix(line, "PING"):
			if _, err := fmt.Fprint(c, "PONG\r\n"); err != nil {
				return
			}
		}
	}
}

// rotate changes the accepted password and drops connected clients, as a
// broker does when a reload removes the password they used.
func (b *authBroker) rotate(password string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.password = password
	for c := range b.conns {
		_, _ = fmt.Fprint(c, "-ERR 'Authorization Violation'\r\n")
		_ = c.Close()
		delete(b.conns, c)
	}
}

func (b *authBroker) accepts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.accepted
}

// The broker and the client's mounted Secret update at different times. The
// client must keep retrying through the authorization errors in between and
// connect once its file has the new password.
func TestClientReconnectsAfterABrokerPasswordRotation(t *testing.T) {
	t.Parallel()
	broker := newAuthBroker(t, "first")
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	js, err := Connect(Config{URL: broker.url(), AllowInsecure: true, Username: "member", PasswordFile: path, Name: "rotation-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { js.nc.Close() })

	broker.rotate("rotated")
	// Several reconnect attempts fail with the old password first.
	time.Sleep(5 * time.Second)
	if js.nc.IsClosed() {
		t.Fatal("client gave up after authorization errors during the rotation")
	}
	if err := os.WriteFile(path, []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for broker.accepts() < 2 || !js.nc.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatalf("client did not reconnect with the rotated password: status %v, last error %v", js.nc.Status(), js.nc.LastError())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := js.nc.RTT(); err != nil {
		t.Fatalf("reconnected client does not answer: %v", err)
	}
}

func writeTestKeyPair(t *testing.T, certFile, keyFile, name string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClientCertificateIsLoadedAgainOnEachHandshake(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeTestKeyPair(t, certFile, keyFile, "first")
	config, err := loadTLS(TLSConfig{CertificateFile: certFile, KeyFile: keyFile, ServerName: "broker"})
	if err != nil {
		t.Fatal(err)
	}
	name := func() string {
		t.Helper()
		cert, err := config.GetClientCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return leaf.Subject.CommonName
	}
	if got := name(); got != "first" {
		t.Fatalf("certificate %q, want first", got)
	}
	writeTestKeyPair(t, certFile, keyFile, "renewed")
	if got := name(); got != "renewed" {
		t.Fatalf("certificate %q after renewal, want renewed", got)
	}
	if err := os.WriteFile(keyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := name(); got != "renewed" {
		t.Fatalf("certificate %q while the key is unreadable, want the last good one", got)
	}
}
