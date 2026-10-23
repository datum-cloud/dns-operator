// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/cache"
	"github.com/coredns/coredns/plugin/file"
	"github.com/coredns/coredns/plugin/forward"
	"github.com/coredns/coredns/plugin/pkg/proxy"
	"github.com/miekg/dns"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type contextState struct {
	pending      bool
	pendingZones map[string]bool
	zones        *file.File
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
	public := s.forward(c.PublicUpstreams)
	for _, definition := range c.Contexts {
		state := &contextState{pending: definition.Pending, pendingZones: map[string]bool{}, zones: &file.File{Zones: file.Zones{Z: map[string]*file.Zone{}}}}
		for _, zone := range definition.Zones {
			apex := model.AbsoluteName(zone.Apex)
			state.pendingZones[apex] = zone.Pending
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
			state.zones.Z[apex] = parsed
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
	for _, upstream := range upstreams {
		p := proxy.NewProxy("dnscontext", upstream, "dns")
		f.SetProxy(p)
		s.proxies = append(s.proxies, p)
	}
	return f
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
	destination := w.LocalAddr()
	if addr, ok := w.RemoteAddr().(interface{ OriginalDestination() net.Addr }); ok {
		destination = addr.OriginalDestination()
	}
	host, _, err := net.SplitHostPort(destination.String())
	if err != nil {
		return outcomeRefusedBadDestination, dns.RcodeRefused, nil
	}
	if p.identityKey != nil {
		if asserted, present := identityDestination(r, p.identityKey); present {
			if !asserted.IsValid() {
				return outcomeRefusedBadIdentity, dns.RcodeRefused, nil
			}
			host = asserted.String()
			destination = &net.TCPAddr{IP: asserted.AsSlice(), Port: 53}
		}
	}
	selected := state.contexts[host]
	if selected == nil {
		return outcomeRefusedUnknownContext, dns.RcodeRefused, nil
	}
	if selected.pending {
		return outcomeServfailContext, dns.RcodeServerFailure, nil
	}
	if len(r.Question) != 1 || r.Opcode != dns.OpcodeQuery || r.Question[0].Qclass != dns.ClassINET {
		return outcomeRefusedUnsupported, dns.RcodeRefused, nil
	}
	if r.Question[0].Qtype == dns.TypeAXFR || r.Question[0].Qtype == dns.TypeIXFR {
		return outcomeRefusedTransfer, dns.RcodeRefused, nil
	}
	w = contextWriter{ResponseWriter: w, destination: destination, request: r, recursionAvailable: selected.next != nil || selected.public != nil}
	apex := plugin.Zones(selected.zones.Names).Matches(strings.ToLower(r.Question[0].Name))
	if apex != "" {
		if selected.pendingZones[apex] {
			return outcomeServfailZone, dns.RcodeServerFailure, nil
		}
		if selected.next != nil {
			rcode, err := selected.next.ServeDNS(ctx, w, r)
			return outcomeForwardedNext, rcode, err
		}
		rcode, err := selected.zones.ServeDNS(ctx, w, r)
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
