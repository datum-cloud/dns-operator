// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	contextdns "go.miloapis.com/dns-operator/internal/internaldns/coredns"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

// Run in Linux with the serving image's dnsdist binary. This exercises the
// generated rules, peer admission, per-query identity and truncation together.
func TestDNSDistUDPIntegration(t *testing.T) {
	bin := os.Getenv("INTERNAL_DNS_TEST_DNSDIST")
	if bin == "" {
		t.Skip("set INTERNAL_DNS_TEST_DNSDIST to run the real dnsdist integration")
	}
	dir := t.TempDir()
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	keyPath := filepath.Join(dir, "identity.key")
	if err := os.WriteFile(keyPath, []byte(key), 0600); err != nil {
		t.Fatal(err)
	}
	freePort := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		return port
	}
	corePort, frontPort := freePort(), freePort()
	coreAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(corePort))
	cfg := contextdns.Config{ListenAddress: coreAddress, ProxyPeers: []string{"127.0.0.1"}, IdentityKeyFile: keyPath}
	bindings := make([]model.Binding, 0, 2)
	for i := 0; i < 2; i++ {
		destination := fmt.Sprintf("127.0.0.%d", i+2)
		zone := fmt.Sprintf("$ORIGIN prod.internal.\n@ 60 IN SOA ns.prod.internal. hostmaster.prod.internal. 1 60 30 3600 2\n@ 60 IN NS ns.prod.internal.\napp 60 IN A 192.0.2.%d\n", i+10)
		for j := 0; j < 8; j++ {
			zone += fmt.Sprintf("large 60 IN TXT %q\n", strings.Repeat(string(rune('a'+i)), 180)+strconv.Itoa(j))
		}
		cfg.Contexts = append(cfg.Contexts, contextdns.Context{ID: destination, Destination: destination, Zones: []contextdns.Zone{{Apex: "prod.internal.", Data: zone}}})
		bindings = append(bindings, model.Binding{BindingUID: destination, ContextUID: destination, ConsumerAddress: destination, Port: uint16(frontPort)})
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	corePath := filepath.Join(dir, "contexts.json")
	if err := os.WriteFile(corePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- contextdns.Run(ctx, corePath, "") }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(7 * time.Second):
			t.Error("CoreDNS did not stop")
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for !listening(coreAddress) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	conf := DNSDistRenderConfig{Path: filepath.Join(dir, "dnsdist.conf"), ListenAddress: fmt.Sprintf("0.0.0.0:%d", frontPort), ACLs: []string{"127.0.0.0/8"}, PoolName: "udp-test", IdentityKeyFile: keyPath, BackendTransport: model.TransportUDP}
	backends := []model.Backend{{MemberID: "core", Address: "127.0.0.1", Port: uint16(corePort)}}
	rules := renderDNSDist(conf, backends, bindings, map[string]bool{"127.0.0.2": true, "127.0.0.3": true}, false, nil, nil)
	if err := os.WriteFile(conf.Path, rules, 0600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(dir, "dnsdist.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--supervised", "--disable-syslog", "-C", conf.Path)
	// The renderer's validation key avoids writing to the system console-key
	// path. It matches the actual CoreDNS fixture key above.
	cmd.Env = append(os.Environ(), "INTERNAL_DNS_VALIDATE=1")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
		if t.Failed() {
			b, _ := os.ReadFile(logFile.Name())
			t.Log(string(b))
		}
	})
	exchange := func(network, destination, name string, qtype uint16) (*dns.Msg, error) {
		q := new(dns.Msg)
		q.SetQuestion(name, qtype)
		q.SetEdns0(512, false)
		// A consumer's forged context must be overwritten by dnsdist.
		q.IsEdns0().Option = append(q.IsEdns0().Option, &dns.EDNS0_LOCAL{Code: contextdns.IdentityOption, Data: []byte("forged")})
		m, _, err := (&dns.Client{Net: network, Timeout: time.Second}).Exchange(q, net.JoinHostPort(destination, strconv.Itoa(frontPort)))
		return m, err
	}
	deadline = time.Now().Add(8 * time.Second)
	for {
		m, err := exchange("udp", "127.0.0.2", "app.prod.internal.", dns.TypeA)
		if err == nil && m.Rcode == dns.RcodeSuccess {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dnsdist UDP backend did not become healthy: %v %v", m, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	count := func(name string) float64 {
		families, err := prometheus.DefaultGatherer.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range families {
			if f.GetName() == name {
				for _, m := range f.Metric {
					for _, l := range m.Label {
						if l.GetName() == "outcome" && l.GetValue() == "accepted" {
							return m.GetCounter().GetValue()
						}
					}
				}
			}
		}
		t.Fatalf("missing metric %s", name)
		return 0
	}
	beforeUDP, beforeTCP := count("internal_dns_transport_datagrams_total"), count("internal_dns_transport_connections_total")
	for i := 0; i < 20; i++ {
		destination := fmt.Sprintf("127.0.0.%d", i%2+2)
		m, err := exchange("udp", destination, "app.prod.internal.", dns.TypeA)
		if err != nil || len(m.Answer) != 1 {
			t.Fatalf("UDP query: %v %v", m, err)
		}
		a, ok := m.Answer[0].(*dns.A)
		if !ok || a.A.String() != fmt.Sprintf("192.0.2.%d", i%2+10) {
			t.Fatalf("UDP crossed contexts: %v", m)
		}
		if opt := m.IsEdns0(); opt != nil {
			for _, o := range opt.Option {
				if o.Option() == contextdns.IdentityOption {
					t.Fatal("identity leaked to client")
				}
			}
		}
	}
	if count("internal_dns_transport_datagrams_total") < beforeUDP+20 || count("internal_dns_transport_connections_total") != beforeTCP {
		t.Fatal("UDP clients still used the TCP backend")
	}
	for i := 0; i < 2; i++ {
		destination := fmt.Sprintf("127.0.0.%d", i+2)
		m, err := exchange("udp", destination, "large.prod.internal.", dns.TypeTXT)
		if err != nil || !m.Truncated || m.Len() > 512 {
			t.Fatalf("UDP truncation: %v %v", m, err)
		}
		m, err = exchange("tcp", destination, "large.prod.internal.", dns.TypeTXT)
		if err != nil || m.Truncated || len(m.Answer) != 8 {
			t.Fatalf("TCP fallback: %v %v", m, err)
		}
		for _, rr := range m.Answer {
			txt, ok := rr.(*dns.TXT)
			if !ok || !strings.HasPrefix(txt.Txt[0], string(rune('a'+i))) {
				t.Fatalf("fallback crossed contexts: %v", m)
			}
		}
	}
	if count("internal_dns_transport_connections_total") <= beforeTCP {
		t.Fatal("TCP fallback did not reach CoreDNS")
	}
	// Let more than maxCheckFailures health-check intervals pass. A healthy
	// keyed listener refuses the unkeyed SOA check; mustResolve=false must
	// keep that backend available rather than marking it down.
	time.Sleep(4 * time.Second)
	if m, err := exchange("udp", "127.0.0.2", "app.prod.internal.", dns.TypeA); err != nil || m.Rcode != dns.RcodeSuccess {
		t.Fatalf("UDP health checks removed the healthy backend: %v %v", m, err)
	}
}
