// SPDX-License-Identifier: AGPL-3.0-only
package coredns

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestDrainPreservesTenantRoutingAndWithdrawal(t *testing.T) {
	p := &ContextPlugin{}
	defer p.Close()
	cfg := fixture()
	if err := p.Replace(cfg); err != nil {
		t.Fatal(err)
	}
	p.draining.Store(true)
	probe := new(dns.Msg)
	probe.SetQuestion(".", dns.TypeSOA)
	w := &answerWriter{destination: "127.0.0.1", network: "tcp"}
	if code, err := p.ServeDNS(context.Background(), w, probe); code != dns.RcodeServerFailure || err != nil {
		t.Fatalf("draining health: %d %v", code, err)
	}
	if answer, _ := query(t, p, "fd53::a", "app.datum.internal."); address(answer) != "192.0.2.10" {
		t.Fatalf("drain interrupted tenant: %v", answer)
	}
	cfg.Contexts = nil
	if err := p.Replace(cfg); err != nil {
		t.Fatal(err)
	}
	if _, code := query(t, p, "fd53::a", "app.datum.internal."); code != dns.RcodeRefused {
		t.Fatal("drain retained withdrawn context")
	}
	// Even an invalid keyed request must take the authentication path, not health.
	p.identityKey = []byte("key")
	probe.SetEdns0(512, false)
	probe.IsEdns0().Option = []dns.EDNS0{&dns.EDNS0_LOCAL{Code: IdentityOption, Data: []byte("wrong")}}
	outcome, code, _ := p.serve(context.Background(), w, probe)
	if outcome != outcomeRefusedBadIdentity || code != dns.RcodeRefused {
		t.Fatalf("bypassed identity: %s %d", outcome, code)
	}
}

func TestProcessDrainsBeforeClosingListeners(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	dir := t.TempDir()
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte(key), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := fixture()
	cfg.ListenAddress = addr
	cfg.IdentityKeyFile = keyPath
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path, "") }()
	exchange := func(network, name string, kind uint16, tenant bool) (*dns.Msg, error) {
		q := new(dns.Msg)
		q.SetQuestion(name, kind)
		if tenant {
			q.SetEdns0(512, false)
			q.IsEdns0().Option = []dns.EDNS0{&dns.EDNS0_LOCAL{Code: IdentityOption, Data: []byte(key + "fd53::a")}}
		}
		m, _, err := (&dns.Client{Net: network, Timeout: 300 * time.Millisecond}).Exchange(q, addr)
		return m, err
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		m, err := exchange("udp", "app.datum.internal.", dns.TypeA, true)
		if err == nil && address(m) == "192.0.2.10" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup: %v %v", m, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	cancel()
	deadline = time.Now().Add(time.Second)
	for {
		m, err := exchange("udp", ".", dns.TypeSOA, false)
		if err == nil && m.Rcode == dns.RcodeServerFailure {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("health did not withdraw: %v %v", m, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, network := range []string{"udp", "tcp"} {
		m, err := exchange(network, "app.datum.internal.", dns.TypeA, true)
		if err != nil || address(m) != "192.0.2.10" {
			t.Fatalf("%s interrupted during drain: %v %v", network, m, err)
		}
		m, err = exchange(network, ".", dns.TypeSOA, false)
		if err != nil || m.Rcode != dns.RcodeServerFailure {
			t.Fatalf("%s health stayed up: %v %v", network, m, err)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(ShutdownTimeout):
		t.Fatal("shutdown stuck")
	}
	if time.Since(start) < DrainDuration {
		t.Fatal("closed before health-check withdrawal window")
	}
}
