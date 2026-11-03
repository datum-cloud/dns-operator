// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func startUDPTestProcess(t *testing.T, c Config, path string) func() {
	t.Helper()
	writeContexts(t, path, c)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path, "") }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(7 * time.Second):
			t.Error("CoreDNS did not stop both listeners")
		}
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(3 * time.Second)
	for {
		m, err := proxyQuery("tcp", c.ListenAddress, "fd53::a")
		if err == nil && address(m) == "192.0.2.10" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("CoreDNS did not start: %v %v", m, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return stop
}

func TestUDPKeyedQueriesIsolationAndTCPFallback(t *testing.T) {
	t.Parallel()
	c, key, path := keyedFixture(t)
	c.IdentityKeyFile = key
	// A complete TXT RRset exceeds the smallest advertised UDP payload.
	for i := range c.Contexts {
		for j := 0; j < 8; j++ {
			c.Contexts[i].Zones[0].Data += "\nlarge 60 IN TXT \"" + strings.Repeat(string(rune('a'+i)), 180) + string(rune('0'+j)) + "\"\n"
		}
	}
	stop := startUDPTestProcess(t, c, path)
	client := &dns.Client{Net: "udp", Timeout: time.Second}
	conn, err := client.Dial(c.ListenAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for i := 0; i < 12; i++ {
		destination, want := "fd53::a", "192.0.2.10"
		if i%2 != 0 {
			destination, want = "fd53::b", "192.0.2.20"
		}
		q := identityQuery(testIdentityKey + destination)
		// Exercise an incoming EDNS query larger than the library's default
		// receive buffer; the advertised answer limit remains 1232 bytes.
		q.IsEdns0().Option = append(q.IsEdns0().Option, &dns.EDNS0_PADDING{Padding: make([]byte, 700)})
		m, _, err := client.ExchangeWithConn(q, conn)
		if err != nil || address(m) != want {
			t.Fatalf("query %d: %v %v", i, m, err)
		}
		if opt := m.IsEdns0(); opt != nil {
			for _, option := range opt.Option {
				if option.Option() == IdentityOption {
					t.Fatal("identity leaked in reply")
				}
			}
		}
	}
	for _, options := range [][]string{nil, {"forged"}, {testIdentityKey + "fd53::a", testIdentityKey + "fd53::b"}, {testIdentityKey + "fd53::c"}} {
		m, _, err := client.ExchangeWithConn(identityQuery(options...), conn)
		if err != nil || m.Rcode != dns.RcodeRefused {
			t.Fatalf("invalid identity admitted: %v %v", m, err)
		}
	}
	for _, destination := range []string{"fd53::a", "fd53::b"} {
		q := identityQuery(testIdentityKey + destination)
		q.Question[0].Name, q.Question[0].Qtype = "large.datum.internal.", dns.TypeTXT
		q.IsEdns0().SetUDPSize(512)
		m, _, err := client.ExchangeWithConn(q, conn)
		if err != nil || !m.Truncated || m.Len() > 512 {
			t.Fatalf("UDP did not truncate: %v %v", m, err)
		}
		m, _, err = (&dns.Client{Net: "tcp", Timeout: time.Second}).Exchange(q, c.ListenAddress)
		if err != nil || m.Truncated || len(m.Answer) != 8 {
			t.Fatalf("TCP retry lost answers: %v %v", m, err)
		}
		prefix := "a"
		if destination == "fd53::b" {
			prefix = "b"
		}
		for _, rr := range m.Answer {
			if txt, ok := rr.(*dns.TXT); !ok || !strings.HasPrefix(txt.Txt[0], prefix) {
				t.Fatalf("TCP retry crossed contexts: %v", rr)
			}
		}
	}
	c.Contexts[0].Zones[0].Data = zoneData("192.0.2.99")
	writeContexts(t, path, c)
	deadline := time.Now().Add(3 * time.Second)
	for {
		m, _, err := client.ExchangeWithConn(identityQuery(testIdentityKey+"fd53::a"), conn)
		if err == nil && address(m) == "192.0.2.99" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("UDP listener did not load the new snapshot: %v %v", m, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	for _, network := range []string{"tcp", "udp"} {
		var close func() error
		if network == "tcp" {
			l, err := net.Listen(network, c.ListenAddress)
			if err != nil {
				t.Fatal(err)
			}
			close = l.Close
		} else {
			l, err := net.ListenPacket(network, c.ListenAddress)
			if err != nil {
				t.Fatal(err)
			}
			close = l.Close
		}
		_ = close()
	}
}

func TestUDPAdmissionDropsUntrustedPeers(t *testing.T) {
	t.Parallel()
	for _, allow := range []bool{false, true} {
		raw, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		m := newTransportMetrics(prometheus.NewRegistry())
		wrapped := &trustedPacketConn{PacketConn: raw, peers: map[netip.Addr]bool{netip.MustParseAddr("127.0.0.1"): allow}, metrics: m}
		server := &dns.Server{PacketConn: wrapped, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) { r := new(dns.Msg); r.SetReply(q); _ = w.WriteMsg(r) })}
		ready := make(chan struct{})
		server.NotifyStartedFunc = func() { close(ready) }
		done := make(chan error, 1)
		go func() { done <- server.ActivateAndServe() }()
		<-ready
		q := identityQuery(testIdentityKey + "fd53::a")
		answer, _, err := (&dns.Client{Net: "udp", Timeout: 100 * time.Millisecond}).Exchange(q, raw.LocalAddr().String())
		if allow {
			if err != nil || answer == nil || testutil.ToFloat64(m.datagramsAccepted) != 1 {
				t.Fatalf("trusted datagram: %v %v", answer, err)
			}
		} else {
			if err == nil || answer != nil || testutil.ToFloat64(m.datagramsUntrusted) != 1 {
				t.Fatalf("untrusted datagram answered: %v %v", answer, err)
			}
		}
		_ = server.Shutdown()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestUDPRequiresKeyAndBindFailureClosesTCP(t *testing.T) {
	t.Parallel()
	c, key, path := keyedFixture(t)
	// A legacy member must work even when another process owns this UDP port.
	packet, err := net.ListenPacket("udp", c.ListenAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = packet.Close() }()
	stop := startUDPTestProcess(t, c, path)
	stop()
	c.IdentityKeyFile = key
	writeContexts(t, path, c)
	if err := Run(context.Background(), path, ""); err == nil || !strings.Contains(err.Error(), "UDP listener") {
		t.Fatalf("UDP bind failure ignored: %v", err)
	}
	l, err := net.Listen("tcp", c.ListenAddress)
	if err != nil {
		t.Fatalf("failed UDP startup leaked TCP listener: %v", err)
	}
	_ = l.Close()
}
