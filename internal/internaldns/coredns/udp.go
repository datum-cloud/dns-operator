// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"context"
	"net"
	"net/netip"

	"github.com/coredns/coredns/core/dnsserver"
	"github.com/miekg/dns"
)

// UDP has no connection-level PROXY identity. Admit only configured peers;
// the plugin must additionally validate the keyed destination on every query.
type trustedPacketConn struct {
	net.PacketConn
	peers   map[netip.Addr]bool
	metrics *transportMetrics
}

func (c *trustedPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, peer, err := c.PacketConn.ReadFrom(b)
		if err != nil {
			return n, peer, err
		}
		if !trusted(c.peers, peer) {
			c.metrics.datagramsUntrusted.Inc()
			continue
		}
		c.metrics.datagramsAccepted.Inc()
		return n, peer, nil
	}
}

func keyedUDPServer(p net.PacketConn, peers map[netip.Addr]bool, core *dnsserver.Server) *dns.Server {
	return &dns.Server{
		PacketConn: &trustedPacketConn{PacketConn: p, peers: peers, metrics: defaultTransportMetrics},
		Net:        "udp",
		// CoreDNS ServePacket uses the DNS library's 512-byte receive default.
		// Keyed EDNS queries can exceed it even when their answers are small.
		UDPSize: dns.MaxMsgSize,
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			ctx := context.WithValue(context.Background(), dnsserver.Key{}, core)
			ctx = context.WithValue(ctx, dnsserver.LoopKey{}, 0)
			core.ServeDNS(ctx, w, r)
		}),
	}
}
