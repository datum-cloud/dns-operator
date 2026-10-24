// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	contextdns "go.miloapis.com/dns-operator/internal/internaldns/coredns"
)

func TestReadinessFailsWhenCoreDNSRefusesDNSDistIdentityTransport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "identity.key")
	if err := os.WriteFile(keyPath, []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", keyPath} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := listener.Addr().String()
		_ = listener.Close()
		path := filepath.Join(dir, "contexts-"+filepath.Base(key)+".json")
		data, _ := json.Marshal(contextdns.Config{ListenAddress: server, ProxyPeers: []string{"127.0.0.1"}, Contexts: []contextdns.Context{}})
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- contextdns.Process{IdentityKeyFile: &key}.Run(ctx, path) }()
		deadline := time.Now().Add(3 * time.Second)
		for !listening(server) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		probeErr := probeIdentityTransport(context.Background(), server)
		cancel()
		<-done
		if key == "" && probeErr == nil {
			t.Fatal("readiness passed while CoreDNS refuses the connections dnsdist sends with an identity key")
		}
		if key != "" && probeErr != nil {
			t.Fatalf("readiness failed against a keyed CoreDNS: %v", probeErr)
		}
	}
}

func listening(server string) bool {
	conn, err := net.DialTimeout("tcp", server, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func TestReadinessCanaryPassesThroughCoreDNSWithZeroContexts(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "identity.key")
	if err := os.WriteFile(keyPath, []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", keyPath} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server := listener.Addr().String()
		_ = listener.Close()
		path := filepath.Join(dir, "contexts-"+filepath.Base(key)+".json")
		data, _ := json.Marshal(contextdns.Config{ListenAddress: server, ProxyPeers: []string{"127.0.0.1"}, Contexts: []contextdns.Context{}})
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- contextdns.Process{IdentityKeyFile: &key}.Run(ctx, path) }()
		deadline := time.Now().Add(3 * time.Second)
		for !listening(server) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		f := newReadinessFixture(t)
		_, portText, _ := net.SplitHostPort(server)
		port, _ := strconv.Atoi(portText)
		f.config.Render = RenderConfig{Role: RenderRegionalCoreDNS, ClusterCoreDNS: CoreDNSRenderConfig{ListenAddress: "127.0.0.1", Port: uint16(port), IdentityKeyFile: key}}
		readyErr := Readiness(context.Background(), f.config)
		cancel()
		<-done
		if readyErr != nil {
			t.Fatalf("CoreDNS with loaded state and no contexts was not ready (identity key %q): %v", key, readyErr)
		}
	}
}

func TestReadinessCanaryQueryCarriesTheIdentityKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "identity.key")
	if err := os.WriteFile(keyPath, []byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := contextdns.LoadIdentityKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := listener.Addr().String()
	_ = listener.Close()
	path := filepath.Join(dir, "contexts.json")
	data, _ := json.Marshal(contextdns.Config{ListenAddress: server, ProxyPeers: []string{"127.0.0.1"}, Contexts: []contextdns.Context{}})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- contextdns.Process{IdentityKeyFile: &keyPath}.Run(ctx, path) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(3 * time.Second)
	for !listening(server) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	for _, tc := range []struct {
		name string
		key  []byte
		want string
	}{
		{"probe key", key, ""},
		{"wrong key", []byte("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB="), "returned SERVFAIL"},
		{"no key", nil, "returned SERVFAIL"},
	} {
		answer, _, err := (&dns.Client{Net: networkTCP, Timeout: time.Second}).Exchange(readinessCanaryQuery(tc.key), server)
		err = readinessCanaryAnswer(answer, err)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("%s: keyed CoreDNS canary = %v, want %q", tc.name, err, tc.want)
		}
	}
}
