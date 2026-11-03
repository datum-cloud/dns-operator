// SPDX-License-Identifier: AGPL-3.0-only
package coredns

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// Run with INTERNAL_DNS_TEST_DNSDIST pointing to the deployed dnsdist binary.
// A real frontend must withdraw the terminating backend before its sockets close.
func TestDNSDistBackendDrainIntegration(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) { testDNSDistBackendDrain(t, transport == "tcp") })
	}
}

func testDNSDistBackendDrain(t *testing.T, tcpOnly bool) {
	bin := os.Getenv("INTERNAL_DNS_TEST_DNSDIST")
	if bin == "" {
		t.Skip("set INTERNAL_DNS_TEST_DNSDIST")
	}
	dir := t.TempDir()
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte(key), 0600); err != nil {
		t.Fatal(err)
	}
	free := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		a := l.Addr().String()
		l.Close()
		return a
	}
	front := free()
	backs := []string{free(), free()}
	cancels := make([]context.CancelFunc, 2)
	dones := make([]chan error, 2)
	for i, addr := range backs {
		cfg := fixture()
		cfg.ListenAddress = addr
		cfg.IdentityKeyFile = keyPath
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, fmt.Sprintf("core%d.json", i))
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		dones[i] = make(chan error, 1)
		done := dones[i]
		go func() { done <- Run(ctx, path, "") }()
	}
	t.Cleanup(func() {
		for _, c := range cancels {
			c()
		}
		for _, d := range dones {
			select {
			case err := <-d:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(ShutdownTimeout):
				t.Error("backend did not stop")
			}
		}
	})
	lua := fmt.Sprintf(`setLocal(%q)
setACL({"127.0.0.0/8"})
setSecurityPollSuffix("")
setServerPolicy(roundrobin)
newServer({address=%q,tcpOnly=%t,checkTCP=%t,checkName=".",checkType="SOA",mustResolve=false,maxCheckFailures=3,rise=2})
newServer({address=%q,tcpOnly=%t,checkTCP=%t,checkName=".",checkType="SOA",mustResolve=false,maxCheckFailures=3,rise=2})
addAction(AllRule(),SetEDNSOptionAction(65431,%q))
`, front, backs[0], tcpOnly, tcpOnly, backs[1], tcpOnly, tcpOnly, key+"fd53::a")
	path := filepath.Join(dir, "dnsdist.conf")
	if err := os.WriteFile(path, []byte(lua), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--supervised", "--disable-syslog", "-C", path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	query := func(network string) error {
		q := new(dns.Msg)
		q.SetQuestion("app.datum.internal.", dns.TypeA)
		m, _, err := (&dns.Client{Net: network, Timeout: 500 * time.Millisecond}).Exchange(q, front)
		if err != nil {
			return err
		}
		if address(m) != "192.0.2.10" {
			return fmt.Errorf("wrong answer: %v", m)
		}
		return nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if query("udp") == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("frontend did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Ensure both backend checks have reached their healthy state.
	time.Sleep(3 * time.Second)
	cancels[0]()
	count := 0
	deadline = time.Now().Add(DrainDuration + 2*time.Second)
	for time.Now().Before(deadline) {
		for _, network := range []string{"udp", "tcp"} {
			if err := query(network); err != nil {
				t.Fatalf("query %d over %s during replacement: %v", count, network, err)
			}
			count++
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("%d successful UDP/TCP queries across backend drain and shutdown", count)
}
