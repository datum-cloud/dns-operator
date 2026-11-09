// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/cache"
	"github.com/coredns/coredns/plugin/file"
	"github.com/coredns/coredns/plugin/forward"
	"github.com/coredns/coredns/plugin/pkg/proxy"
	"github.com/coredns/coredns/plugin/pkg/upstream"
	"github.com/miekg/dns"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type contextState struct {
	pending      bool
	pendingZones map[string]bool
	zoneApexes   map[string]struct{}
	zones        *file.File
	zoneFiles    map[string]*file.File
	next         plugin.Handler
	public       plugin.Handler
}

type snapshot struct {
	contexts map[string]*contextState
	proxies  []*proxy.Proxy
}

func (s *snapshot) close() {
	for _, upstreamProxy := range s.proxies {
		upstreamProxy.Stop()
	}
}

type ContextPlugin struct {
	state       atomic.Pointer[snapshot]
	identityKey []byte
}

func (p *ContextPlugin) Name() string { return "dnscontext" }

func compile(c Config) (*snapshot, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	s := &snapshot{contexts: map[string]*contextState{}}
	public := s.orderedForward(c.PublicUpstreams)
	for _, definition := range c.Contexts {
		state := &contextState{pending: definition.Pending, pendingZones: map[string]bool{}, zoneApexes: map[string]struct{}{}, zones: &file.File{Zones: file.Zones{Z: map[string]*file.Zone{}}}, zoneFiles: map[string]*file.File{}}
		for _, zone := range definition.Zones {
			apex := model.AbsoluteName(zone.Apex)
			state.pendingZones[apex] = zone.Pending
			state.zoneApexes[apex] = struct{}{}
			state.zones.Names = append(state.zones.Names, apex)
			if zone.Pending || len(definition.Upstreams) > 0 {
				state.zones.Z[apex] = nil
				continue
			}
			parsed, err := file.Parse(strings.NewReader(zone.Data), apex, "publication", -1)
			if err != nil {
				s.close()
				return nil, err
			}
			parsed.Upstream = upstream.New()
			state.zones.Z[apex] = parsed
			zoneApex, zoneData := apex, parsed
			state.zoneFiles[apex] = &file.File{ZoneLookupFunc: func(string) (string, *file.Zone, bool) {
				return zoneApex, zoneData, true
			}}
		}
		state.next = s.forward(definition.Upstreams)
		if public != nil && state.next == nil {
			cached := cache.New()
			cached.Next = public
			state.public = cached
		}
		s.contexts[netip.MustParseAddr(definition.Destination).Unmap().String()] = state
	}
	return s, nil
}

func (s *snapshot) forward(upstreams []string) plugin.Handler {
	if len(upstreams) == 0 {
		return nil
	}
	f := forward.New()
	for _, address := range upstreams {
		f.SetProxy(s.newProxy(address))
	}
	return f
}

// orderedForward tries upstreams in the listed order. CoreDNS forward picks
// among its proxies at random and has no exported way to change that.
func (s *snapshot) orderedForward(upstreams []string) plugin.Handler {
	if len(upstreams) <= 1 {
		return s.forward(upstreams)
	}
	ordered := make(orderedUpstreams, 0, len(upstreams))
	for i, address := range upstreams {
		f := forward.New()
		p := s.newProxy(address)
		if i < len(upstreams)-1 {
			p.SetReadTimeout(preferredUpstreamReadTimeout)
		}
		f.SetProxy(p)
		ordered = append(ordered, &orderedUpstream{proxy: p, handler: f})
	}
	return ordered
}

func (s *snapshot) newProxy(address string) *proxy.Proxy {
	p := proxy.NewProxy("dnscontext", address, "dns")
	s.proxies = append(s.proxies, p)
	return p
}

// Two attempts at the preferred upstream must leave the last upstream time to
// answer inside the two-second UDP timeout of the dnsdist in front of CoreDNS.
const (
	preferredUpstreamReadTimeout = 700 * time.Millisecond
	preferredUpstreamBackoff     = 5 * time.Second
	preferredUpstreamFailures    = 3
	upstreamMaxFails             = 2
)

type orderedUpstream struct {
	proxy      *proxy.Proxy
	handler    plugin.Handler
	retryAfter atomic.Int64
	failures   atomic.Int32
}

func (u *orderedUpstream) available(now time.Time) bool {
	return !u.proxy.Down(upstreamMaxFails) && now.UnixNano() >= u.retryAfter.Load()
}

type orderedUpstreams []*orderedUpstream

func (orderedUpstreams) Name() string { return "dnscontext-public" }

// A SERVFAIL moves only this query along. Backing off takes consecutive
// transport failures, so one slow public name cannot divert everyone else's
// traffic.
func (u orderedUpstreams) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	var (
		rcode int
		err   error
		last  *dns.Msg
	)
	for i, candidate := range u {
		now := time.Now()
		if i < len(u)-1 && !candidate.available(now) {
			continue
		}
		captured := &capturedResponse{ResponseWriter: w}
		rcode, err = candidate.handler.ServeDNS(ctx, captured, r)
		if err != nil && candidate.failures.Add(1) >= preferredUpstreamFailures {
			candidate.failures.Store(0)
			candidate.retryAfter.Store(now.Add(preferredUpstreamBackoff).UnixNano())
		}
		if captured.response != nil {
			last = captured.response
			if last.Rcode != dns.RcodeServerFailure {
				candidate.failures.Store(0)
				candidate.retryAfter.Store(0)
				return dns.RcodeSuccess, w.WriteMsg(last)
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	if last != nil {
		return dns.RcodeSuccess, w.WriteMsg(last)
	}
	return rcode, err
}

type capturedResponse struct {
	dns.ResponseWriter
	response *dns.Msg
}

func (c *capturedResponse) WriteMsg(m *dns.Msg) error {
	c.response = m
	return nil
}

func (p *ContextPlugin) Replace(c Config) error {
	s, err := compile(c)
	if err != nil {
		snapshotReloads.WithLabelValues("rejected").Inc()
		return err
	}
	observeSnapshot(s)
	if previous := p.state.Swap(s); previous != nil {
		previous.close()
	}
	return nil
}

func (p *ContextPlugin) Close() {
	if previous := p.state.Swap(nil); previous != nil {
		previous.close()
	}
}

func (p *ContextPlugin) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	outcome, rcode, err := p.serve(ctx, w, r)
	outcomeQueries[outcome].Inc()
	return rcode, err
}

func (p *ContextPlugin) serve(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (string, int, error) {
	state := p.state.Load()
	if state == nil {
		return outcomeServfailNoState, dns.RcodeServerFailure, nil
	}
	if IsReadinessCanary(r) {
		if p.identityKey != nil {
			if asserted, present := identityDestination(r, p.identityKey); !present || !asserted.IsValid() {
				return outcomeServfailCanaryIdentity, dns.RcodeServerFailure, nil
			}
		}
		answer := new(dns.Msg)
		answer.SetReply(r)
		answer.Authoritative = true
		answer.Answer = []dns.RR{&dns.TXT{Hdr: dns.RR_Header{Name: ReadinessCanaryName, Rrtype: dns.TypeTXT, Class: dns.ClassCHAOS}, Txt: []string{ReadinessCanaryText}}}
		return outcomeReadinessCanary, dns.RcodeSuccess, w.WriteMsg(answer)
	}
	contextCanary := IsContextReadinessCanary(r)
	if !contextCanary && (len(r.Question) != 1 || r.Question[0].Qclass != dns.ClassINET) {
		return outcomeRefusedUnsupported, dns.RcodeRefused, nil
	}
	host, destination, outcome := p.destination(ctx, w, r)
	if outcome != "" {
		return outcome, dns.RcodeRefused, nil
	}
	selected := state.contexts[host]
	if selected == nil {
		return outcomeRefusedUnknownContext, dns.RcodeRefused, nil
	}
	if selected.pending {
		return outcomeServfailContext, dns.RcodeServerFailure, nil
	}
	if contextCanary {
		if selected.next != nil {
			rcode, err := selected.next.ServeDNS(ctx, w, r)
			return outcomeForwardedNext, rcode, err
		}
		answer := new(dns.Msg)
		answer.SetReply(r)
		answer.Authoritative = true
		answer.Answer = []dns.RR{&dns.TXT{Hdr: dns.RR_Header{Name: ContextReadinessCanaryName, Rrtype: dns.TypeTXT, Class: dns.ClassCHAOS}, Txt: []string{ReadinessCanaryText}}}
		return outcomeContextCanary, dns.RcodeSuccess, w.WriteMsg(answer)
	}
	if len(r.Question) != 1 || r.Opcode != dns.OpcodeQuery || r.Question[0].Qclass != dns.ClassINET {
		return outcomeRefusedUnsupported, dns.RcodeRefused, nil
	}
	if r.Question[0].Qtype == dns.TypeAXFR || r.Question[0].Qtype == dns.TypeIXFR {
		return outcomeRefusedTransfer, dns.RcodeRefused, nil
	}
	w = contextWriter{ResponseWriter: w, destination: destination, request: r, recursionAvailable: selected.next != nil || selected.public != nil}
	ctx = context.WithValue(ctx, selectedDestinationKey{}, selectedDestination{host: host, destination: destination})
	apex := matchZone(selected.zoneApexes, canonicalQueryName(r.Question[0].Name))
	if apex != "" {
		if selected.pendingZones[apex] {
			return outcomeServfailZone, dns.RcodeServerFailure, nil
		}
		if selected.next != nil {
			rcode, err := selected.next.ServeDNS(ctx, w, r)
			return outcomeForwardedNext, rcode, err
		}
		zones := selected.zones
		if selected := selected.zoneFiles[apex]; selected != nil {
			zones = selected
		}
		rcode, err := zones.ServeDNS(ctx, w, r)
		return outcomeServedZone, rcode, err
	}
	if selected.next != nil {
		rcode, err := selected.next.ServeDNS(ctx, w, r)
		return outcomeForwardedNext, rcode, err
	}
	if selected.public == nil {
		return outcomeServfailNoUpstream, dns.RcodeServerFailure, errors.New("public recursive upstream is unavailable")
	}
	rcode, err := selected.public.ServeDNS(ctx, w, r)
	return outcomeForwardedPublic, rcode, err
}

// CoreDNS follows a CNAME that leaves its zone by running a copy of the query
// through the server again with the same context. The copy has no identity
// option of its own, since the original was stripped, so the follow-up keeps
// the destination the original query selected. Only this plugin sets the
// value, so a client cannot supply it.
type selectedDestinationKey struct{}

type selectedDestination struct {
	host        string
	destination net.Addr
}

func (p *ContextPlugin) destination(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (string, net.Addr, string) {
	if selected, ok := ctx.Value(selectedDestinationKey{}).(selectedDestination); ok {
		return selected.host, selected.destination, ""
	}
	destination := w.LocalAddr()
	if addr, ok := w.RemoteAddr().(interface{ OriginalDestination() net.Addr }); ok {
		destination = addr.OriginalDestination()
	}
	host, _, err := net.SplitHostPort(destination.String())
	if err != nil {
		return "", nil, outcomeRefusedBadDestination
	}
	if p.identityKey != nil {
		asserted, present := identityDestination(r, p.identityKey)
		if _, udp := w.RemoteAddr().(*net.UDPAddr); udp && !present {
			return "", nil, outcomeRefusedBadIdentity
		}
		if present {
			if !asserted.IsValid() {
				return "", nil, outcomeRefusedBadIdentity
			}
			host = asserted.String()
			destination = &net.TCPAddr{IP: asserted.AsSlice(), Port: 53}
		}
	}
	return host, destination, ""
}

func canonicalQueryName(name string) string {
	return strings.ToLower(dns.Fqdn(name))
}

// matchZone finds the most-specific configured apex by walking DNS label
// boundaries from the complete query name toward the root. Both the query and
// the immutable apex set are canonical when the snapshot is compiled, so this
// avoids comparing and splitting the query once for every configured zone.
func matchZone(apexes map[string]struct{}, qname string) string {
	for offset := 0; offset < len(qname); {
		if _, ok := apexes[qname[offset:]]; ok {
			return qname[offset:]
		}
		next, end := dns.NextLabel(qname, offset)
		if end {
			break
		}
		offset = next
	}
	if _, ok := apexes["."]; ok {
		return "."
	}
	return ""
}

// The canary is answered before a context is selected, so it proves a loaded
// snapshot even on a member that serves no contexts. With an identity key it
// also proves the sender holds the key. Any client that reaches a context can
// read it; it reveals only that this process has loaded state.
const (
	ReadinessCanaryName = "ready.internal-dns."
	ReadinessCanaryText = "ready"
)

// ContextReadinessCanaryName is answered after context selection, so it proves
// one context is installed and not pending without touching public recursion.
const ContextReadinessCanaryName = "context-ready.internal-dns."

func IsReadinessCanary(r *dns.Msg) bool { return isCanary(r, ReadinessCanaryName) }

func IsContextReadinessCanary(r *dns.Msg) bool { return isCanary(r, ContextReadinessCanaryName) }

func isCanary(r *dns.Msg, name string) bool {
	return r.Opcode == dns.OpcodeQuery && len(r.Question) == 1 && r.Question[0].Qclass == dns.ClassCHAOS &&
		r.Question[0].Qtype == dns.TypeTXT && strings.EqualFold(r.Question[0].Name, name)
}

type contextWriter struct {
	dns.ResponseWriter
	destination        net.Addr
	request            *dns.Msg
	recursionAvailable bool
}

func (w contextWriter) LocalAddr() net.Addr { return w.destination }

func (w contextWriter) WriteMsg(message *dns.Msg) error {
	message = message.Copy()
	if message.Authoritative {
		message.RecursionAvailable = w.recursionAvailable
	}
	if w.RemoteAddr().Network() == "udp" {
		size := uint16(dns.MinMsgSize)
		if opt := w.request.IsEdns0(); opt != nil {
			size = max(size, min(opt.UDPSize(), 4096))
		}
		message.Truncate(int(size))
	}
	return w.ResponseWriter.WriteMsg(message)
}
