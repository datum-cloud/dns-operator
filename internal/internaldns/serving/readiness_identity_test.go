// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

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
