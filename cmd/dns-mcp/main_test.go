// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

const testServerName = "dns-mcp.datum-dns-system.svc.cluster.local"

func writeSelfSignedCert(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "dns-mcp"},
		DNSNames:              []string{testServerName},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("writing certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}

	pool = x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	return certFile, keyFile, pool
}

// startServer runs serve on a loopback port and returns its address and a
// stop function that shuts it down and reports what serve returned.
func startServer(t *testing.T, certFile, keyFile string) (addr string, stop func() error) {
	t.Helper()

	handler, err := newHandler(&rest.Config{Host: "https://milo.invalid"})
	if err != nil {
		t.Fatalf("building handler: %v", err)
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, listener, handler, certFile, keyFile) }()

	stopped := false
	stop = func() error {
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(shutdownTimeout):
			t.Fatal("serve did not return after its context was cancelled")
			return nil
		}
	}
	t.Cleanup(func() {
		if !stopped {
			_ = stop()
		}
	})
	return listener.Addr().String(), stop
}

func getHealthz(t *testing.T, client *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	return client.Do(req)
}

func TestServe_ServesHTTPSWhenCertAndKeyAreSet(t *testing.T) {
	certFile, keyFile, pool := writeSelfSignedCert(t)
	addr, stop := startServer(t, certFile, keyFile)

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: testServerName, MinVersion: tls.VersionTLS12},
	}}
	resp, err := getHealthz(t, client, "https://"+addr+"/healthz")
	if err != nil {
		t.Fatalf("GET over https: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		t.Fatalf("GET /healthz = %d %q, want 200 ok", resp.StatusCode, body)
	}
	if resp.TLS == nil {
		t.Fatal("response carried no TLS connection state")
	}

	plain, err := getHealthz(t, &http.Client{}, "http://"+addr+"/healthz")
	if err == nil {
		_ = plain.Body.Close()
		if plain.StatusCode == http.StatusOK {
			t.Fatal("plain http request succeeded against a TLS listener")
		}
	}

	if err := stop(); err != nil {
		t.Fatalf("serve returned %v on shutdown, want nil", err)
	}
}

func TestServe_ServesPlainHTTPWhenNoCertIsSet(t *testing.T) {
	addr, stop := startServer(t, "", "")

	resp, err := getHealthz(t, &http.Client{}, "http://"+addr+"/healthz")
	if err != nil {
		t.Fatalf("GET over http: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", resp.StatusCode)
	}
	if resp.TLS != nil {
		t.Fatal("plain http response carried TLS connection state")
	}

	if err := stop(); err != nil {
		t.Fatalf("serve returned %v on shutdown, want nil", err)
	}
}

func TestRun_FailsWhenOnlyOneTLSFileIsSet(t *testing.T) {
	certFile, keyFile, _ := writeSelfSignedCert(t)

	tests := []struct {
		name              string
		certFile, keyFile string
	}{
		{name: "certificate without key", certFile: certFile},
		{name: "key without certificate", keyFile: keyFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(t.Context(), "127.0.0.1:0", tt.certFile, tt.keyFile, &rest.Config{Host: "https://milo.invalid"})
			if err == nil {
				t.Fatal("run started with half a TLS configuration, want an error")
			}
			if !strings.Contains(err.Error(), "--tls-cert-file and --tls-key-file must be set together") {
				t.Fatalf("run error = %q, want it to name both flags", err)
			}
		})
	}
}

func TestServe_FailsWhenCertificateCannotBeLoaded(t *testing.T) {
	dir := t.TempDir()
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	err = serve(t.Context(), listener, http.NotFoundHandler(),
		filepath.Join(dir, "missing.crt"), filepath.Join(dir, "missing.key"))
	if err == nil || !strings.Contains(err.Error(), "loading TLS certificate") {
		t.Fatalf("serve error = %v, want a certificate load failure", err)
	}
}
