package serving

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestResolverProbeAddressMatchesServingTier(t *testing.T) {
	t.Parallel()
	binding := model.Binding{ConsumerAddress: "2001:db8:1::53", ClusterAddress: "2001:db8:2::53", Port: 53}
	if got := resolverProbeServer(DNSVerifierConfig{}, binding); got != "[2001:db8:1::53]:53" {
		t.Fatalf("node probe server = %q", got)
	}
	if got := resolverProbeServer(DNSVerifierConfig{UseClusterAddress: true}, binding); got != "[2001:db8:2::53]:53" {
		t.Fatalf("cluster probe server = %q", got)
	}
}

func TestResolverVerifierRetriesTransientPoolWarmup(t *testing.T) {
	t.Parallel()
	udpAttempts := 0
	exchange := func(_ context.Context, network, _ string, name string, _ time.Duration) (*dns.Msg, error) {
		if network == "udp" {
			udpAttempts++
			if udpAttempts == 1 {
				return nil, errors.New("connection refused")
			}
			if udpAttempts == 2 {
				return &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeServerFailure}}, nil
			}
		}
		return &dns.Msg{
			MsgHdr: dns.MsgHdr{Rcode: dns.RcodeSuccess},
			Answer: []dns.RR{&dns.SOA{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 30}}},
		}, nil
	}
	if err := verifyResolverPath(context.Background(), "[fd00::53]:53", []string{"private.example."}, time.Second, exchange); err != nil {
		t.Fatal(err)
	}
	if udpAttempts != 3 {
		t.Fatalf("UDP attempts = %d, want transport error + SERVFAIL + success", udpAttempts)
	}
}

func TestResolverVerifierBoundsWarmupRetries(t *testing.T) {
	t.Parallel()
	started := time.Now()
	err := verifyResolverPath(context.Background(), "127.0.0.1:53", []string{"private.example."}, 40*time.Millisecond, func(context.Context, string, string, string, time.Duration) (*dns.Msg, error) {
		return &dns.Msg{MsgHdr: dns.MsgHdr{Rcode: dns.RcodeServerFailure}}, nil
	})
	if err == nil {
		t.Fatal("permanent SERVFAIL was accepted")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("bounded verifier took %s", elapsed)
	}
}

func TestPublicationVerifierProvesMemberContextAndFingerprintOverBothTransports(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		t.Run(fmt.Sprint(wrong), func(t *testing.T) {
			tcp, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tcp.Close() }()
			udp, err := net.ListenPacket("udp", tcp.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = udp.Close() }()
			seen := make(chan string, 2)
			response := func(wire []byte, network string) []byte {
				if len(wire) < 28 || wire[12] != 0x21 || !bytes.Equal(wire[20:24], []byte{10, 20, 0, 1}) {
					t.Errorf("probe lost protected destination: %x", wire)
					return nil
				}
				request := new(dns.Msg)
				payload := wire[28:]
				if network == "tcp" {
					payload = payload[2:]
				}
				if err := request.Unpack(payload); err != nil {
					t.Error(err)
					return nil
				}
				reply := new(dns.Msg)
				reply.SetReply(request)
				reply.Authoritative = true
				mailbox := "a.b.publication.internal."
				if wrong {
					mailbox = "old.b.publication.internal."
				}
				reply.Answer = []dns.RR{&dns.SOA{Hdr: dns.RR_Header{Name: "prod.internal.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 5}, Ns: "ns1.internal-dns.invalid.", Mbox: mailbox, Serial: 7}}
				data, _ := reply.Pack()
				seen <- network
				return data
			}
			go func() {
				buf := make([]byte, 65535)
				n, peer, err := udp.ReadFrom(buf)
				if err != nil {
					return
				}
				data := response(buf[:n], "udp")
				_, _ = udp.WriteTo(data, peer)
			}()
			go func() {
				conn, err := tcp.Accept()
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				header := make([]byte, 28, 30)
				if _, err := io.ReadFull(conn, header); err != nil {
					return
				}
				size := make([]byte, 2)
				if _, err := io.ReadFull(conn, size); err != nil {
					return
				}
				body := make([]byte, binary.BigEndian.Uint16(size))
				if _, err := io.ReadFull(conn, body); err != nil {
					return
				}
				data := response(append(append(header, size...), body...), "tcp")
				_, _ = conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(data))), data...))
			}()
			verifier := PublicationDNSVerifier{Config: PublicationDNSVerifierConfig{Server: tcp.Addr().String(), Timeout: time.Second}}
			err = verifier.VerifyPublication(context.Background(), model.PublicationPlan{Apex: "prod.internal"}, []PublicationView{{View: "context-a", DestinationAddress: "10.20.0.1", DestinationPort: 53, Serial: 7, FingerprintMailbox: "a.b.publication.internal."}})
			if wrong {
				if err == nil {
					t.Fatal("old publication mailbox accepted despite matching serial")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(seen) != 2 {
				t.Fatal("both DNS transports were not proved")
			}
		})
	}
}

func TestPublicationVerifierHasOneDeadlineForAllContextsAndTransports(t *testing.T) {
	t.Parallel()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tcp.Close() }()
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = udp.Close() }()
	reply := func(wire []byte) []byte {
		request := new(dns.Msg)
		if err := request.Unpack(wire); err != nil {
			t.Error(err)
			return nil
		}
		answer := new(dns.Msg)
		answer.SetReply(request)
		answer.Authoritative = true
		answer.Answer = []dns.RR{&dns.SOA{Hdr: dns.RR_Header{Name: "prod.internal.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 5}, Ns: "ns1.internal-dns.invalid.", Mbox: "a.b.publication.internal.", Serial: 1}}
		data, _ := answer.Pack()
		return data
	}
	// Two complete slow successes precede an unresponsive third context. A
	// per-exchange budget takes at least 300ms; one overall budget takes 100ms.
	go func() {
		for i := 0; i < 3; i++ {
			buf := make([]byte, 65535)
			n, peer, err := udp.ReadFrom(buf)
			if err != nil {
				return
			}
			if i == 2 {
				return
			}
			time.Sleep(50 * time.Millisecond)
			_, _ = udp.WriteTo(reply(buf[28:n]), peer)
		}
	}()
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}
			header := make([]byte, 30)
			if _, err := io.ReadFull(conn, header); err != nil {
				_ = conn.Close()
				return
			}
			body := make([]byte, binary.BigEndian.Uint16(header[28:]))
			if _, err := io.ReadFull(conn, body); err != nil {
				_ = conn.Close()
				return
			}
			time.Sleep(50 * time.Millisecond)
			data := reply(body)
			_, _ = conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(data))), data...))
			_ = conn.Close()
		}
	}()
	views := make([]PublicationView, 100)
	for i := range views {
		views[i] = PublicationView{View: fmt.Sprint(i), DestinationAddress: "10.20.0.1", DestinationPort: 53, Serial: 1, FingerprintMailbox: "a.b.publication.internal."}
	}
	verifier := PublicationDNSVerifier{Config: PublicationDNSVerifierConfig{Server: tcp.Addr().String(), Timeout: 100 * time.Millisecond}}
	started := time.Now()
	if err := verifier.VerifyPublication(context.Background(), model.PublicationPlan{Apex: "prod.internal"}, views); err == nil {
		t.Fatal("unresponsive member was verified")
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("context count reset query deadline: %s", elapsed)
	}
}

func TestPublicationVerifierRetriesExactVersionOnBothTransports(t *testing.T) {
	t.Parallel()
	view := PublicationView{View: "context-a", Serial: 7, FingerprintMailbox: "new.publication.internal."}
	attempts := map[string]int{}
	err := verifyPublicationViews(context.Background(), "member:53", "prod.internal.", []PublicationView{view}, time.Second,
		func(_ context.Context, network, _, _ string, _ PublicationView, _ time.Duration) (*dns.Msg, error) {
			attempts[network]++
			if attempts[network] == 1 {
				return nil, nil
			}
			serial, mailbox := view.Serial, view.FingerprintMailbox
			switch attempts[network] {
			case 2:
				serial--
			case 3:
				mailbox = "old.publication.internal."
			}
			return &dns.Msg{MsgHdr: dns.MsgHdr{Authoritative: true}, Answer: []dns.RR{&dns.SOA{Serial: serial, Mbox: mailbox}}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if attempts["udp"] != 4 || attempts["tcp"] != 4 {
		t.Fatalf("exact proof retries = %v, want nil + old serial + old fingerprint + success per transport", attempts)
	}
}

func TestPublicationVerifierBoundsRepeatedMismatchesAndReportsContext(t *testing.T) {
	t.Parallel()
	view := PublicationView{View: "context-stale", Serial: 7, FingerprintMailbox: "new.publication.internal."}
	started := time.Now()
	err := verifyPublicationViews(context.Background(), "member:53", "prod.internal.", []PublicationView{view}, 70*time.Millisecond,
		func(context.Context, string, string, string, PublicationView, time.Duration) (*dns.Msg, error) {
			return &dns.Msg{MsgHdr: dns.MsgHdr{Authoritative: true}, Answer: []dns.RR{&dns.SOA{Serial: 6, Mbox: "old.publication.internal."}}}, nil
		})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "context-stale over udp") || !strings.Contains(err.Error(), "last response") {
		t.Fatalf("mismatch deadline diagnostics = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("mismatch retries reset deadline: %s", elapsed)
	}
}

func TestPublicationVerifierRejectsSuccessAfterDeadline(t *testing.T) {
	t.Parallel()
	view := PublicationView{View: "context-late", Serial: 7, FingerprintMailbox: "new.publication.internal."}
	err := verifyPublicationViews(context.Background(), "member:53", "prod.internal.", []PublicationView{view}, 10*time.Millisecond,
		func(ctx context.Context, _, _, _ string, _ PublicationView, _ time.Duration) (*dns.Msg, error) {
			<-ctx.Done()
			return &dns.Msg{MsgHdr: dns.MsgHdr{Authoritative: true}, Answer: []dns.RR{&dns.SOA{Serial: view.Serial, Mbox: view.FingerprintMailbox}}}, nil
		})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("late response proved readiness: %v", err)
	}
}

func TestZeroZoneContextProvesRecursionOverUDPAndTCP(t *testing.T) {
	tcp, err := net.Listen(networkTCP, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket(networkUDP, tcp.Addr().String())
	if err != nil {
		_ = tcp.Close()
		t.Fatal(err)
	}
	seen := make(chan string, 2)
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		if len(r.Question) != 1 || r.Question[0].Name != "." || r.Question[0].Qtype != dns.TypeSOA {
			t.Errorf("unsafe recursion probe: %#v", r.Question)
		}
		reply := new(dns.Msg)
		reply.SetReply(r)
		reply.Answer = []dns.RR{&dns.SOA{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 5}, Ns: "a.root-servers.net.", Mbox: "nstld.verisign-grs.com.", Serial: 1, Refresh: 60, Retry: 30, Expire: 3600, Minttl: 5}}
		if err := w.WriteMsg(reply); err != nil {
			t.Error(err)
		}
		seen <- w.RemoteAddr().Network()
	})
	tcpServer := &dns.Server{Listener: tcp, Handler: handler}
	udpServer := &dns.Server{PacketConn: udp, Handler: handler}
	go func() { _ = tcpServer.ActivateAndServe() }()
	go func() { _ = udpServer.ActivateAndServe() }()
	t.Cleanup(func() { _ = tcpServer.Shutdown(); _ = udpServer.Shutdown() })
	address := tcp.Addr().(*net.TCPAddr)
	binding := model.Binding{ConsumerAddress: "127.0.0.1", Port: uint16(address.Port)}
	if err := (DNSVerifier{Config: DNSVerifierConfig{Timeout: time.Second}}).Verify(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("probe count=%d", len(seen))
	}
}
