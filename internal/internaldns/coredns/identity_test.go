// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

const testIdentityKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func identityQuery(options ...string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion("app.datum.internal.", dns.TypeA)
	m.SetEdns0(1232, false)
	opt := m.IsEdns0()
	for _, data := range options {
		opt.Option = append(opt.Option, &dns.EDNS0_LOCAL{Code: IdentityOption, Data: []byte(data)})
	}
	opt.Option = append(opt.Option, &dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: "0123456789abcdef"})
	return m
}

func TestIdentityOptionSelectsContextOnlyWithKey(t *testing.T) {
	t.Parallel()
	p := &ContextPlugin{identityKey: []byte(testIdentityKey)}
	defer p.Close()
	if err := p.Replace(fixture()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		options []string
		rcode   int
		want    string
	}{
		{"keyed", []string{testIdentityKey + "fd53::b"}, dns.RcodeSuccess, "192.0.2.20"},
		{"forged alongside keyed", []string{"BBBB" + "fd53::a", testIdentityKey + "fd53::b"}, dns.RcodeSuccess, "192.0.2.20"},
		{"forged", []string{strings.Repeat("B", len(testIdentityKey)) + "fd53::b"}, dns.RcodeRefused, ""},
		{"bare destination", []string{"fd53::b"}, dns.RcodeRefused, ""},
		{"two keyed", []string{testIdentityKey + "fd53::a", testIdentityKey + "fd53::b"}, dns.RcodeRefused, ""},
		{"keyed unknown context", []string{testIdentityKey + "fd53::c"}, dns.RcodeRefused, ""},
		{"absent", nil, dns.RcodeRefused, ""},
	} {
		w := &answerWriter{destination: "127.0.0.1"}
		m := identityQuery(tc.options...)
		rcode, err := p.ServeDNS(context.Background(), w, m)
		if err != nil {
			t.Fatal(tc.name, err)
		}
		if rcode != tc.rcode || address(w.answer) != tc.want {
			t.Fatalf("%s: rcode %d answer %v", tc.name, rcode, w.answer)
		}
		opt := m.IsEdns0()
		if len(opt.Option) != 1 || opt.Option[0].Option() != dns.EDNS0COOKIE {
			t.Fatalf("%s: identity options were not removed before the query left the plugin: %v", tc.name, opt.Option)
		}
	}
}

func TestIdentityOptionIsIgnoredWithoutKey(t *testing.T) {
	t.Parallel()
	p := &ContextPlugin{}
	defer p.Close()
	if err := p.Replace(fixture()); err != nil {
		t.Fatal(err)
	}
	w := &answerWriter{destination: "fd53::a"}
	if _, err := p.ServeDNS(context.Background(), w, identityQuery(testIdentityKey+"fd53::b")); err != nil {
		t.Fatal(err)
	}
	if address(w.answer) != "192.0.2.10" {
		t.Fatalf("an identity option changed the context of a member without a key: %v", w.answer)
	}
}

func TestPlainConnectionsCarryKeyedIdentityAndProxyStillWorks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "identity.key")
	if err := os.WriteFile(keyPath, []byte(testIdentityKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := fixture()
	c.ListenAddress = listener.Addr().String()
	c.IdentityKeyFile = keyPath
	_ = listener.Close()
	path := filepath.Join(dir, "contexts.json")
	data, _ := json.Marshal(c)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path, "") }()
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
	var conn *dns.Conn
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err = dns.DialTimeout("tcp", c.ListenAddress, 200*time.Millisecond)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	for i := 0; i < 4; i++ {
		destination, want := "fd53::a", "192.0.2.10"
		if i%2 == 1 {
			destination, want = "fd53::b", "192.0.2.20"
		}
		if err := conn.WriteMsg(identityQuery(testIdentityKey + destination)); err != nil {
			t.Fatal(err)
		}
		m, err := conn.ReadMsg()
		if err != nil || address(m) != want {
			t.Fatalf("query %d on a reused connection: %v %v", i, m, err)
		}
	}
	if err := conn.WriteMsg(identityQuery()); err != nil {
		t.Fatal(err)
	}
	if m, err := conn.ReadMsg(); err != nil || m.Rcode != dns.RcodeRefused {
		t.Fatalf("a query without identity reached a context: %v %v", m, err)
	}
	if m, err := proxyQuery("tcp", c.ListenAddress, "fd53::a"); err != nil || address(m) != "192.0.2.10" {
		t.Fatalf("PROXY probes stopped working: %v %v", m, err)
	}
}

func TestPlainConnectionsNeedAnIdentityKey(t *testing.T) {
	t.Parallel()
	for _, plain := range []bool{false, true} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		proxy := &proxyListener{Listener: listener, peers: map[netip.Addr]bool{netip.MustParseAddr("127.0.0.1"): true}, plain: plain}
		accepted := make(chan net.Conn, 1)
		go func() {
			conn, err := proxy.Accept()
			if err == nil {
				accepted <- conn
			}
		}()
		client, err := dns.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if err := client.WriteMsg(identityQuery()); err != nil {
			t.Fatal(err)
		}
		select {
		case conn := <-accepted:
			if !plain {
				t.Fatal("connection without a PROXY header accepted while no identity key is configured")
			}
			m, err := (&dns.Conn{Conn: conn}).ReadMsg()
			if err != nil || m.Question[0].Name != "app.datum.internal." {
				t.Fatalf("plain connection lost the bytes read while looking for a PROXY header: %v %v", m, err)
			}
			_ = conn.Close()
		case <-time.After(500 * time.Millisecond):
			if plain {
				t.Fatal("plain connection was not accepted")
			}
		}
		_ = client.Close()
		_ = proxy.Close()
	}
}
