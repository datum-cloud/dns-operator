package serving

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/miekg/dns"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

const (
	networkTCP = "tcp"
	networkUDP = "udp"
)

type DNSVerifierConfig struct {
	Timeout           time.Duration `json:"timeout,omitempty"`
	PublicName        string        `json:"publicName,omitempty"`
	UseClusterAddress bool          `json:"useClusterAddress,omitempty"`
}
type DNSVerifier struct{ Config DNSVerifierConfig }

type resolverExchange func(context.Context, string, string, string, time.Duration) (*dns.Msg, error)

// Verify probes the complete consumer-facing path over both required DNS
// transports. Every attached private apex must return a non-failure response.
func (v DNSVerifier) Verify(ctx context.Context, b model.Binding) error {
	timeout := v.Config.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	names := []string{}
	for _, z := range b.Zones {
		names = append(names, model.AbsoluteName(z.Apex))
	}
	if v.Config.PublicName != "" {
		names = append(names, model.AbsoluteName(v.Config.PublicName))
	}
	if len(names) == 0 {
		// A recursion-only context has no private SOA to prove. The root SOA
		// exercises recursive resolution without choosing a product domain.
		names = append(names, ".")
	}
	server := resolverProbeServer(v.Config, b)
	return verifyResolverPath(ctx, server, names, timeout, exchangeResolverSOA)
}

func resolverProbeServer(config DNSVerifierConfig, binding model.Binding) string {
	address := binding.ConsumerAddress
	if config.UseClusterAddress {
		address = binding.ClusterAddress
	}
	return net.JoinHostPort(address, strconv.Itoa(int(binding.Port)))
}

func exchangeResolverSOA(ctx context.Context, network, server, name string, timeout time.Duration) (*dns.Msg, error) {
	client := &dns.Client{Net: network, Timeout: timeout}
	msg := new(dns.Msg)
	msg.SetQuestion(name, dns.TypeSOA)
	answer, _, err := client.ExchangeContext(ctx, msg, server)
	return answer, err
}

func verifyResolverPath(ctx context.Context, server string, names []string, timeout time.Duration, exchange resolverExchange) error {
	deadline := time.Now().Add(timeout)
	for _, network := range []string{networkUDP, networkTCP} {
		for _, name := range names {
			var last error
			for {
				remaining := time.Until(deadline)
				if remaining <= 0 {
					return fmt.Errorf("%s probe %s through %s did not become ready within %s: %w", network, name, server, timeout, last)
				}
				attemptTimeout := min(remaining, 250*time.Millisecond)
				answer, err := exchange(ctx, network, server, name, attemptTimeout)
				switch {
				case err != nil:
					last = fmt.Errorf("exchange: %w", err)
				case answer == nil:
					last = errors.New("empty DNS response")
				case answer.Rcode != dns.RcodeSuccess:
					last = fmt.Errorf("returned %s", dns.RcodeToString[answer.Rcode])
				case !hasSOA(answer):
					last = fmt.Errorf("returned no authoritative SOA (rcode %s)", dns.RcodeToString[answer.Rcode])
				default:
					last = nil
				}
				if last == nil {
					break
				}
				delay := min(100*time.Millisecond, time.Until(deadline))
				if delay <= 0 {
					continue
				}
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		}
	}
	return nil
}

type PublicationView struct {
	View               string
	DestinationAddress string
	DestinationPort    uint16
	Serial             uint32
	FingerprintMailbox string
}
type PublicationVerifier interface {
	VerifyPublication(context.Context, model.PublicationPlan, []PublicationView) error
}
type PublicationDNSVerifierConfig struct {
	Server  string        `json:"server"`
	Timeout time.Duration `json:"timeout,omitempty"`
}
type PublicationDNSVerifier struct{ Config PublicationDNSVerifierConfig }

// Probe the member's BIND process directly with the protected destination
// identity. Querying the shared dnsdist pool could falsely prove another member.
func (v PublicationDNSVerifier) VerifyPublication(ctx context.Context, plan model.PublicationPlan, views []PublicationView) error {
	if v.Config.Server == "" {
		return errors.New("regional BIND probe server is required")
	}
	if len(views) == 0 {
		return errors.New("publication is not installed in any authorized context")
	}
	timeout := v.Config.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return verifyPublicationViews(ctx, v.Config.Server, model.AbsoluteName(plan.Apex), views, timeout, exchangePublication)
}

type publicationExchange func(context.Context, string, string, string, PublicationView, time.Duration) (*dns.Msg, error)

// BIND reload may return before every view exposes its new zone. Retry the
// exact installed fingerprint while retaining one deadline for the entire proof.
func verifyPublicationViews(ctx context.Context, server, apex string, views []PublicationView, timeout time.Duration, exchange publicationExchange) error {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline, _ := probeCtx.Deadline()
	for _, view := range views {
		for _, network := range []string{networkUDP, networkTCP} {
			var last error
			for {
				if err := publicationDeadlineError(probeCtx, deadline); err != nil {
					return fmt.Errorf("verify BIND view %s over %s within %s: %w (last response: %v)", view.View, network, timeout, err, last)
				}
				answer, err := exchange(probeCtx, network, server, apex, view, min(time.Until(deadline), 250*time.Millisecond))
				if err != nil {
					last = fmt.Errorf("exchange: %w", err)
				} else {
					last = publicationAnswerError(answer, view)
				}
				// An exchange finishing after the total deadline cannot prove readiness.
				if err := publicationDeadlineError(probeCtx, deadline); err != nil {
					return fmt.Errorf("verify BIND view %s over %s within %s: %w (last response: %v)", view.View, network, timeout, err, last)
				}
				if last == nil {
					break
				}
				timer := time.NewTimer(min(25*time.Millisecond, time.Until(deadline)))
				select {
				case <-probeCtx.Done():
					timer.Stop()
					return fmt.Errorf("verify BIND view %s over %s within %s: %w (last response: %v)", view.View, network, timeout, probeCtx.Err(), last)
				case <-timer.C:
				}
			}
		}
	}
	return nil
}

func publicationDeadlineError(ctx context.Context, deadline time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func publicationAnswerError(answer *dns.Msg, view PublicationView) error {
	if answer == nil {
		return errors.New("empty DNS response")
	}
	if answer.Rcode != dns.RcodeSuccess || !answer.Authoritative {
		return errors.New("did not return authoritative success")
	}
	var observed []string
	for _, rr := range answer.Answer {
		if soa, ok := rr.(*dns.SOA); ok {
			if soa.Serial == view.Serial && soa.Mbox == view.FingerprintMailbox {
				return nil
			}
			observed = append(observed, fmt.Sprintf("serial %d fingerprint %s", soa.Serial, soa.Mbox))
		}
	}
	return fmt.Errorf("did not return installed publication serial %d and fingerprint %s; returned SOAs %v", view.Serial, view.FingerprintMailbox, observed)
}

func exchangePublication(ctx context.Context, network, server, name string, view PublicationView, timeout time.Duration) (*dns.Msg, error) {
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, server)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	msg := new(dns.Msg)
	msg.SetQuestion(name, dns.TypeSOA)
	wire, err := msg.Pack()
	if err != nil {
		return nil, err
	}
	destination, err := netip.ParseAddr(view.DestinationAddress)
	if err != nil {
		return nil, err
	}
	// Source is immaterial for context selection; destination identity is trusted
	// only because the direct peer is in BIND's protected proxy ACL.
	header := []byte{13, 10, 13, 10, 0, 13, 10, 81, 85, 73, 84, 10, 0x21, 0, 0, 0}
	proto := byte(2)
	if network == networkTCP {
		proto = 1
	}
	if destination.Is4() {
		header[13] = 0x10 | proto
		header[15] = 12
		header = append(header, 127, 0, 0, 1)
		ip := destination.As4()
		header = append(header, ip[:]...)
	} else {
		header[13] = 0x20 | proto
		header[15] = 36
		source := netip.IPv6Loopback().As16()
		header = append(header, source[:]...)
		ip := destination.As16()
		header = append(header, ip[:]...)
	}
	header = binary.BigEndian.AppendUint16(header, 53000)
	header = binary.BigEndian.AppendUint16(header, view.DestinationPort)
	if network == networkTCP {
		header = binary.BigEndian.AppendUint16(header, uint16(len(wire)))
	}
	if _, err := conn.Write(append(header, wire...)); err != nil {
		return nil, err
	}
	var reply []byte
	if network == networkTCP {
		size := make([]byte, 2)
		if _, err := io.ReadFull(conn, size); err != nil {
			return nil, err
		}
		reply = make([]byte, binary.BigEndian.Uint16(size))
		if _, err := io.ReadFull(conn, reply); err != nil {
			return nil, err
		}
	} else {
		buf := make([]byte, 65535)
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		reply = buf[:n]
	}
	answer := new(dns.Msg)
	if err := answer.Unpack(reply); err != nil {
		return nil, err
	}
	if answer.Id != msg.Id {
		return nil, errors.New("DNS probe transaction ID mismatch")
	}
	return answer, nil
}
func hasSOA(answer *dns.Msg) bool {
	for _, rr := range append(answer.Answer, answer.Ns...) {
		if rr.Header().Rrtype == dns.TypeSOA {
			return true
		}
	}
	return false
}
