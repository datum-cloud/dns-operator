// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func zoneData(address string) string {
	return "$ORIGIN datum.internal.\n@ 2 IN SOA ns.internal. hostmaster.internal. 1 60 30 3600 2\n" +
		"@ 2 IN NS ns.internal.\napp 2 IN A " + address + "\n"
}

func fixture() Config {
	return Config{ListenAddress: "127.0.0.1:5300", ProxyPeers: []string{"127.0.0.1"}, Contexts: []Context{
		{ID: "a", Destination: "fd53::a", Zones: []Zone{{Apex: "datum.internal.", Data: zoneData("192.0.2.10")}}},
		{ID: "b", Destination: "fd53::b", Zones: []Zone{{Apex: "datum.internal.", Data: zoneData("192.0.2.20")}}},
	}}
}

type answerWriter struct {
	dns.ResponseWriter
	destination string
	network     string
	answer      *dns.Msg
}

func (w *answerWriter) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.ParseIP(w.destination), Port: 53}
}
func (w *answerWriter) RemoteAddr() net.Addr {
	if w.network == "tcp" {
		return &net.TCPAddr{IP: net.ParseIP("10.0.0.10"), Port: 1234}
	}
	return &net.UDPAddr{IP: net.ParseIP("10.0.0.10"), Port: 1234}
}
func (w *answerWriter) WriteMsg(m *dns.Msg) error { w.answer = m; return nil }

func query(t *testing.T, plugin *ContextPlugin, destination, name string) (*dns.Msg, int) {
	t.Helper()
	w := &answerWriter{destination: destination}
	m := new(dns.Msg)
	m.SetQuestion(name, dns.TypeA)
	rcode, err := plugin.ServeDNS(context.Background(), w, m)
	if err != nil {
		t.Fatal(err)
	}
	return w.answer, rcode
}

func address(m *dns.Msg) string {
	if m != nil {
		for _, rr := range m.Answer {
			if a, ok := rr.(*dns.A); ok {
				return a.A.String()
			}
		}
	}
	return ""
}

func TestOverlappingZonesAndClientAddressesRemainIsolated(t *testing.T) {
	t.Parallel()
	p := &ContextPlugin{}
	defer p.Close()
	if err := p.Replace(fixture()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		for _, tc := range []struct{ destination, want string }{{"fd53::a", "192.0.2.10"}, {"fd53::b", "192.0.2.20"}} {
			answer, _ := query(t, p, tc.destination, "app.datum.internal.")
			if address(answer) != tc.want {
				t.Fatalf("%s received %v", tc.destination, answer)
			}
		}
	}
	if _, code := query(t, p, "fd53::c", "app.datum.internal."); code != dns.RcodeRefused {
		t.Fatal("unknown context was served")
	}
}

func TestInvalidPublicationRetainsStateAndWithdrawalIsImmediate(t *testing.T) {
	t.Parallel()
	p := &ContextPlugin{}
	defer p.Close()
	c := fixture()
	if err := p.Replace(c); err != nil {
		t.Fatal(err)
	}
	c.Contexts[0].Zones[0].Data = "invalid"
	if err := p.Replace(c); err == nil {
		t.Fatal("invalid zone was accepted")
	}
	answer, _ := query(t, p, "fd53::a", "app.datum.internal.")
	if address(answer) != "192.0.2.10" {
		t.Fatal("invalid publication disturbed accepted state")
	}
	c.Contexts[0].Zones[0].Data = strings.ReplaceAll(zoneData("192.0.2.10"), "app 2 IN A 192.0.2.10\n", "")
	if err := p.Replace(c); err != nil {
		t.Fatal(err)
	}
	answer, _ = query(t, p, "fd53::a", "app.datum.internal.")
	if answer == nil || answer.Rcode != dns.RcodeNameError || address(answer) != "" {
		t.Fatal("withdrawn endpoint remained available")
	}
	answer, _ = query(t, p, "fd53::b", "app.datum.internal.")
	if address(answer) != "192.0.2.20" {
		t.Fatal("withdrawal changed another context")
	}
	c.Contexts[0].Zones[0].Pending = true
	if err := p.Replace(c); err != nil {
		t.Fatal(err)
	}
	if _, code := query(t, p, "fd53::a", "missing.datum.internal."); code != dns.RcodeServerFailure {
		t.Fatal("pending private zone did not block public fallback")
	}
}

func TestPublicCachesRemainIsolatedAndPrivateMissesStayLocal(t *testing.T) {
	t.Parallel()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var value atomic.Int32
	value.Store(1)
	var queries atomic.Int32
	server := &dns.Server{PacketConn: packet, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		queries.Add(1)
		answer := new(dns.Msg)
		answer.SetReply(r)
		answer.RecursionAvailable = true
		answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(198, 51, 100, byte(value.Load()))}}
		_ = w.WriteMsg(answer)
	})}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown(); _ = packet.Close() })
	c := fixture()
	c.PublicUpstreams = []string{packet.LocalAddr().String()}
	p := &ContextPlugin{}
	defer p.Close()
	if err := p.Replace(c); err != nil {
		t.Fatal(err)
	}
	answer, _ := query(t, p, "fd53::a", "public.example.")
	if address(answer) != "198.51.100.1" {
		t.Fatal("public recursion failed", answer)
	}
	value.Store(2)
	answer, _ = query(t, p, "fd53::b", "public.example.")
	if address(answer) != "198.51.100.2" {
		t.Fatal("another context's cached answer was returned", answer)
	}
	answer, _ = query(t, p, "fd53::a", "public.example.")
	if address(answer) != "198.51.100.1" || queries.Load() != 2 {
		t.Fatal("context cache was not retained", answer, queries.Load())
	}
	answer, _ = query(t, p, "fd53::a", "missing.datum.internal.")
	if answer == nil || answer.Rcode != dns.RcodeNameError || queries.Load() != 2 {
		t.Fatal("private miss reached public recursion", answer, queries.Load())
	}
}

func TestTrustedPROXYDestinationSurvivesTCP(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := fixture()
	c.ListenAddress = listener.Addr().String()
	for i := range c.Contexts {
		c.Contexts[i].Zones[0].Data += "alias 2 IN CNAME target.apps.internal.\n"
		address := []string{"192.0.2.10", "192.0.2.20"}[i]
		c.Contexts[i].Zones = append(c.Contexts[i].Zones, Zone{Apex: "apps.internal.", Data: "$ORIGIN apps.internal.\n@ 2 IN SOA ns.internal. hostmaster.internal. 1 60 30 3600 2\n@ 2 IN NS ns.internal.\ntarget 2 IN A " + address + "\n"})
	}
	_ = listener.Close()
	path := filepath.Join(t.TempDir(), "contexts.json")
	write := func(c Config) {
		t.Helper()
		data, _ := json.Marshal(c)
		if err := os.WriteFile(path+".tmp", data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			t.Fatal(err)
		}
	}
	write(c)
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
	for _, network := range []string{"tcp", "tcp-udp"} {
		for _, tc := range []struct{ destination, want string }{{"fd53::a", "192.0.2.10"}, {"fd53::b", "192.0.2.20"}} {
			deadline := time.Now().Add(3 * time.Second)
			for {
				m, err := proxyQuery(network, c.ListenAddress, tc.destination)
				if err == nil && address(m) == tc.want {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s %s: %v %v", network, tc.destination, m, err)
				}
				time.Sleep(20 * time.Millisecond)
			}
			m, err := proxyQuery(network, c.ListenAddress, tc.destination, "alias.datum.internal.")
			if err != nil || address(m) != tc.want {
				t.Fatalf("CNAME changed context over %s: %v %v", network, m, err)
			}
		}
	}
	if reply, err := proxyQuery("udp", c.ListenAddress, "fd53::b"); err == nil {
		t.Fatalf("UDP PROXY reached a tenant context: %v", reply)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	m, err := proxyQuery("tcp", c.ListenAddress, "fd53::a")
	if err != nil || address(m) != "192.0.2.10" {
		t.Fatal("missing delivery file disturbed accepted state", m, err)
	}
	c.Contexts[0].Zones[0].Data = zoneData("192.0.2.30")
	write(c)
	deadline := time.Now().Add(3 * time.Second)
	for {
		m, err := proxyQuery("tcp", c.ListenAddress, "fd53::a")
		if err == nil && address(m) == "192.0.2.30" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("publication did not activate", m, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func proxyQuery(network, server, destination string, names ...string) (*dns.Msg, error) {
	originalUDP := network == "tcp-udp"
	if originalUDP {
		network = "tcp"
	}
	conn, err := net.DialTimeout(network, server, 200*time.Millisecond)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
	header := append([]byte(nil), proxySignature...)
	family := byte(0x22)
	if network == "tcp" && !originalUDP {
		family = 0x21
	}
	header = append(header, 0x21, family, 0, 36)
	header = append(header, net.ParseIP("fd00::10").To16()...)
	header = append(header, net.ParseIP(destination).To16()...)
	header = append(header, 0x04, 0xd2, 0, 53)
	message := new(dns.Msg)
	name := "app.datum.internal."
	if len(names) > 0 {
		name = names[0]
	}
	message.SetQuestion(name, dns.TypeA)
	if network == "tcp" {
		if _, err := conn.Write(header); err != nil {
			return nil, err
		}
		dnsConn := &dns.Conn{Conn: conn}
		if err := dnsConn.WriteMsg(message); err != nil {
			return nil, err
		}
		return dnsConn.ReadMsg()
	}
	data, _ := message.Pack()
	if _, err := conn.Write(append(header, data...)); err != nil {
		return nil, err
	}
	buffer := make([]byte, 4096)
	n, err := conn.Read(buffer)
	if err != nil {
		return nil, err
	}
	answer := new(dns.Msg)
	return answer, answer.Unpack(buffer[:n])
}

func semanticFixture() Config {
	root := `$ORIGIN datum.internal.
@ 60 IN SOA ns.datum.internal. hostmaster.datum.internal. 1 60 30 3600 5
@ 60 IN NS ns.datum.internal.
ns 60 IN A 192.0.2.53
app 60 IN A 192.0.2.10
alias 60 IN CNAME app.datum.internal.
*.wild 60 IN A 192.0.2.40
leaf.branch 60 IN A 192.0.2.41
old 60 IN DNAME new.datum.internal.
x.new 60 IN A 192.0.2.42
delegated 60 IN NS ns.delegated.datum.internal.
ns.delegated 60 IN A 192.0.2.43
signed 60 IN A 192.0.2.44
signed 60 IN RRSIG A 8 2 60 20300101000000 20200101000000 12345 datum.internal. AQ==
big 60 IN TXT "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
`
	nested := `$ORIGIN apps.datum.internal.
@ 60 IN SOA ns.apps.datum.internal. hostmaster.datum.internal. 1 60 30 3600 5
@ 60 IN NS ns.apps.datum.internal.
ns 60 IN A 192.0.2.54
app 60 IN A 198.51.100.10
`
	other := strings.ReplaceAll(root, "192.0.2.10", "192.0.2.20")
	return Config{ListenAddress: "127.0.0.1:5300", ProxyPeers: []string{"127.0.0.1"}, Contexts: []Context{
		{ID: "a", Destination: "fd53::a", Zones: []Zone{
			{Apex: "datum.internal.", Data: root},
			{Apex: "apps.datum.internal.", Data: nested},
			{Apex: "pending.datum.internal.", Pending: true},
		}},
		{ID: "b", Destination: "fd53::b", Zones: []Zone{{Apex: "datum.internal.", Data: other}}},
	}}
}

func serveMessage(p *ContextPlugin, destination, network string, message *dns.Msg) (*dns.Msg, int, error) {
	w := &answerWriter{destination: destination, network: network}
	rcode, err := p.ServeDNS(context.Background(), w, message.Copy())
	return w.answer, rcode, err
}

func responseBytes(t *testing.T, message *dns.Msg) []byte {
	t.Helper()
	if message == nil {
		return nil
	}
	message = message.Copy()
	message.Id = 0
	data, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSelectedZoneLookupMatchesFileFallback(t *testing.T) {
	optimized := &ContextPlugin{}
	defer optimized.Close()
	if err := optimized.Replace(semanticFixture()); err != nil {
		t.Fatal(err)
	}
	fallback := &ContextPlugin{}
	defer fallback.Close()
	if err := fallback.Replace(semanticFixture()); err != nil {
		t.Fatal(err)
	}
	for _, state := range fallback.state.Load().contexts {
		state.zoneFiles = nil
	}
	tests := []struct {
		name, destination, qname, network string
		qtype                             uint16
		do                                bool
		udpSize                           uint16
	}{
		{"tenant a", "fd53::a", "APP.DATUM.INTERNAL.", "udp", dns.TypeA, false, 0},
		{"tenant b", "fd53::b", "app.datum.internal.", "udp", dns.TypeA, false, 0},
		{"most specific apex", "fd53::a", "app.apps.datum.internal.", "udp", dns.TypeA, false, 0},
		{"pending apex", "fd53::a", "name.pending.datum.internal.", "udp", dns.TypeA, false, 0},
		{"wildcard", "fd53::a", "x.wild.datum.internal.", "udp", dns.TypeA, false, 0},
		{"empty non terminal", "fd53::a", "branch.datum.internal.", "udp", dns.TypeA, false, 0},
		{"cname", "fd53::a", "alias.datum.internal.", "udp", dns.TypeA, false, 0},
		{"dname", "fd53::a", "x.old.datum.internal.", "udp", dns.TypeA, false, 0},
		{"delegation", "fd53::a", "host.delegated.datum.internal.", "udp", dns.TypeA, false, 0},
		{"nxdomain", "fd53::a", "missing.datum.internal.", "udp", dns.TypeA, false, 0},
		{"nodata", "fd53::a", "app.datum.internal.", "udp", dns.TypeAAAA, false, 0},
		{"dnssec do", "fd53::a", "signed.datum.internal.", "udp", dns.TypeA, true, 1232},
		{"edns truncation", "fd53::a", "big.datum.internal.", "udp", dns.TypeTXT, false, 512},
		{"tcp untruncated", "fd53::a", "big.datum.internal.", "tcp", dns.TypeTXT, false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			message := new(dns.Msg)
			message.SetQuestion(tc.qname, tc.qtype)
			if tc.udpSize != 0 {
				message.SetEdns0(tc.udpSize, tc.do)
			}
			want, wantCode, wantErr := serveMessage(fallback, tc.destination, tc.network, message)
			got, gotCode, gotErr := serveMessage(optimized, tc.destination, tc.network, message)
			if gotCode != wantCode || (gotErr == nil) != (wantErr == nil) || string(responseBytes(t, got)) != string(responseBytes(t, want)) {
				t.Fatalf("optimized response differs from fallback\ncode=%s want=%s\nerror=%v want=%v\nresponse=%v\nwant=%v", dns.RcodeToString[gotCode], dns.RcodeToString[wantCode], gotErr, wantErr, got, want)
			}
		})
	}
}

func TestSelectedZoneLookupDoesNotRepeatZoneMatching(t *testing.T) {
	p := &ContextPlugin{}
	defer p.Close()
	if err := p.Replace(semanticFixture()); err != nil {
		t.Fatal(err)
	}
	selected := p.state.Load().contexts["fd53::a"]
	delete(selected.zones.Z, "apps.datum.internal.")
	answer, code := query(t, p, "fd53::a", "app.apps.datum.internal.")
	if code != dns.RcodeSuccess || address(answer) != "198.51.100.10" {
		t.Fatalf("selected apex was looked up again: code=%s answer=%v", dns.RcodeToString[code], answer)
	}
}

func TestConcurrentReplaceAndSelectedZoneLookup(t *testing.T) {
	p := &ContextPlugin{}
	defer p.Close()
	if err := p.Replace(fixture()); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 16)
	var queries sync.WaitGroup
	for i := 0; i < 8; i++ {
		queries.Add(1)
		go func() {
			defer queries.Done()
			for j := 0; j < 250; j++ {
				message := new(dns.Msg)
				message.SetQuestion("app.datum.internal.", dns.TypeA)
				answer, code, err := serveMessage(p, "fd53::a", "udp", message)
				got := address(answer)
				if err != nil || code != dns.RcodeSuccess || got != "192.0.2.10" && got != "192.0.2.30" {
					errs <- fmt.Errorf("code=%s address=%s error=%v", dns.RcodeToString[code], got, err)
					return
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		config := fixture()
		if i%2 != 0 {
			config.Contexts[0].Zones[0].Data = zoneData("192.0.2.30")
		}
		if err := p.Replace(config); err != nil {
			t.Fatal(err)
		}
	}
	queries.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func benchmarkContextPlugin(b *testing.B, selected bool) {
	zones := make([]Zone, 128)
	for i := range zones {
		apex := fmt.Sprintf("z%d.internal.", i)
		zones[i] = Zone{Apex: apex, Data: fmt.Sprintf("$ORIGIN %s\n@ 60 IN SOA ns.%s hostmaster.%s 1 60 30 3600 5\n@ 60 IN NS ns.%s\nns 60 IN A 192.0.2.53\napp 60 IN A 192.0.2.10\n", apex, apex, apex, apex)}
	}
	config := Config{ListenAddress: "127.0.0.1:5300", ProxyPeers: []string{"127.0.0.1"}, Contexts: []Context{{ID: "a", Destination: "fd53::a", Zones: zones}}}
	p := &ContextPlugin{}
	if err := p.Replace(config); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(p.Close)
	if !selected {
		p.state.Load().contexts["fd53::a"].zoneFiles = nil
	}
	message := new(dns.Msg)
	message.SetQuestion("app.z127.internal.", dns.TypeA)
	w := &answerWriter{destination: "fd53::a"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.answer = nil
		if code, err := p.ServeDNS(context.Background(), w, message); err != nil || code != dns.RcodeSuccess {
			b.Fatalf("code=%s error=%v", dns.RcodeToString[code], err)
		}
	}
}

func BenchmarkContextPluginZoneLookup(b *testing.B) {
	b.Run("selected", func(b *testing.B) { benchmarkContextPlugin(b, true) })
	b.Run("fallback", func(b *testing.B) { benchmarkContextPlugin(b, false) })
}

func TestMalformedAndWrongTransportHeadersAreRejected(t *testing.T) {
	t.Parallel()
	for _, data := range [][]byte{nil, make([]byte, 16), append(append([]byte(nil), proxySignature...), 0x22, 0x22, 0, 0)} {
		if _, _, _, err := parseHeader(data, false); err == nil {
			t.Fatal("invalid PROXY identity accepted")
		}
	}
	header := append(append([]byte(nil), proxySignature...), 0x21, 0x12, 0, 12)
	header = append(header, net.ParseIP("192.0.2.1").To4()...)
	header = append(header, net.ParseIP("192.0.2.2").To4()...)
	header = append(header, 0, 53, 0, 53)
	if _, _, _, err := parseHeader(header, true); err != nil {
		t.Fatalf("forwarded UDP metadata rejected on TCP connection: %v", err)
	}
	header[13] = 0x13
	if _, _, _, err := parseHeader(header, true); err == nil {
		t.Fatal("unknown client protocol accepted")
	}
	header[13] = 0x12
	binary.BigEndian.PutUint16(header[14:16], 65535)
	if _, _, _, err := parseHeader(header, false); err == nil {
		t.Fatal("truncated PROXY header accepted")
	}
	if trusted(map[netip.Addr]bool{}, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 53}) {
		t.Fatal("untrusted proxy peer accepted")
	}
}

func TestLocalHealthHeaderCannotSelectAContext(t *testing.T) {
	t.Parallel()
	header := append(append([]byte(nil), proxySignature...), 0x20, 0, 0, 0)
	for _, tcp := range []bool{false, true} {
		source, destination, size, err := parseHeader(header, tcp)
		if err != nil || source != nil || destination != nil || size != len(header) {
			t.Fatal("LOCAL header acquired an asserted tenant identity")
		}
	}
	c := fixture()
	c.Contexts[0].Destination = "127.0.0.1"
	if err := c.Validate(); err == nil {
		t.Fatal("management listener can select a tenant context")
	}
}

func TestReadinessCanaryReportsLoadedStateWithoutAContext(t *testing.T) {
	t.Parallel()
	canary := func(p *ContextPlugin, destination string, class uint16) (*dns.Msg, int) {
		w := &answerWriter{destination: destination}
		m := new(dns.Msg)
		m.SetQuestion("READY.internal-dns.", dns.TypeTXT)
		m.Question[0].Qclass = class
		rcode, err := p.ServeDNS(context.Background(), w, m)
		if err != nil {
			t.Fatal(err)
		}
		return w.answer, rcode
	}
	p := &ContextPlugin{}
	if _, rcode := canary(p, "fd53::a", dns.ClassCHAOS); rcode != dns.RcodeServerFailure {
		t.Fatalf("canary without loaded state returned %s", dns.RcodeToString[rcode])
	}
	c := fixture()
	c.Contexts = []Context{}
	if err := p.Replace(c); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, destination := range []string{"fd53::a", "127.0.0.1"} {
		answer, rcode := canary(p, destination, dns.ClassCHAOS)
		if rcode != dns.RcodeSuccess || answer == nil || answer.Rcode != dns.RcodeSuccess || len(answer.Answer) != 1 {
			t.Fatalf("canary through %s returned %s: %v", destination, dns.RcodeToString[rcode], answer)
		}
		txt, ok := answer.Answer[0].(*dns.TXT)
		if !ok || txt.Hdr.Class != dns.ClassCHAOS || strings.Join(txt.Txt, "") != ReadinessCanaryText {
			t.Fatalf("canary answer %v", answer.Answer[0])
		}
	}
	if _, rcode := canary(p, "fd53::a", dns.ClassINET); rcode != dns.RcodeRefused {
		t.Fatalf("IN-class canary name bypassed context selection: %s", dns.RcodeToString[rcode])
	}
	p.Close()
	if _, rcode := canary(p, "fd53::a", dns.ClassCHAOS); rcode != dns.RcodeServerFailure {
		t.Fatalf("canary after state was withdrawn returned %s", dns.RcodeToString[rcode])
	}
}

func TestKeyedReadinessCanaryRequiresTheIdentityKey(t *testing.T) {
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
	}{
		{"keyed", []string{testIdentityKey + "::1"}, dns.RcodeSuccess},
		{"forged", []string{strings.Repeat("B", len(testIdentityKey)) + "::1"}, dns.RcodeServerFailure},
		{"two keyed", []string{testIdentityKey + "::1", testIdentityKey + "fd53::a"}, dns.RcodeServerFailure},
		{"absent", nil, dns.RcodeServerFailure},
	} {
		w := &answerWriter{destination: "fd53::a"}
		m := identityQuery(tc.options...)
		m.Question[0] = dns.Question{Name: ReadinessCanaryName, Qtype: dns.TypeTXT, Qclass: dns.ClassCHAOS}
		rcode, err := p.ServeDNS(context.Background(), w, m)
		if err != nil {
			t.Fatal(tc.name, err)
		}
		if rcode != tc.rcode {
			t.Fatalf("%s: canary returned %s, want %s", tc.name, dns.RcodeToString[rcode], dns.RcodeToString[tc.rcode])
		}
	}
}

func TestNonINQueriesAreRefusedBeforeContextSelection(t *testing.T) {
	t.Parallel()
	p := &ContextPlugin{}
	defer p.Close()
	c := fixture()
	c.Contexts[0].Pending = true
	if err := p.Replace(c); err != nil {
		t.Fatal(err)
	}
	for _, class := range []uint16{dns.ClassCHAOS, dns.ClassANY, dns.ClassNONE} {
		w := &answerWriter{destination: "fd53::a"}
		m := new(dns.Msg)
		m.SetQuestion("version.bind.", dns.TypeTXT)
		m.Question[0].Qclass = class
		rcode, err := p.ServeDNS(context.Background(), w, m)
		if err != nil {
			t.Fatal(err)
		}
		if rcode != dns.RcodeRefused {
			t.Fatalf("class %d query to a pending context returned %s", class, dns.RcodeToString[rcode])
		}
	}
}
