// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"reflect"
	"time"

	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
)

func Run(ctx context.Context, path string) error {
	previous, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	cfg, err := Decode(previous)
	if err != nil {
		return err
	}
	p := &ContextPlugin{}
	if err := p.Replace(cfg); err != nil {
		return err
	}
	defer p.Close()
	host, port, _ := net.SplitHostPort(cfg.ListenAddress)
	server, err := dnsserver.NewServer("dns://"+cfg.ListenAddress, []*dnsserver.Config{{
		Zone: ".", Port: port, Transport: "dns", ListenHosts: []string{host},
		Plugin: []plugin.Plugin{func(plugin.Handler) plugin.Handler { return p }},
	}})
	if err != nil {
		return err
	}
	peers := map[netip.Addr]bool{}
	for _, peer := range cfg.ProxyPeers {
		peers[netip.MustParseAddr(peer).Unmap()] = true
	}
	tcp, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return err
	}
	defer func() { _ = tcp.Close() }()
	udp, err := net.ListenPacket("udp", cfg.ListenAddress)
	if err != nil {
		return err
	}
	defer func() { _ = udp.Close() }()
	done := make(chan error, 2)
	go func() { done <- server.Serve(&proxyListener{Listener: tcp, peers: peers}) }()
	go func() { done <- server.ServePacket(&proxyPacketConn{PacketConn: udp, peers: peers}) }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return server.Stop()
		case err := <-done:
			_ = server.Stop()
			return fmt.Errorf("CoreDNS listener stopped: %w", errors.Join(err, errors.New("unexpected listener exit")))
		case <-ticker.C:
			data, err := os.ReadFile(path)
			if err != nil || bytes.Equal(previous, data) {
				continue
			}
			next, err := Decode(data)
			if err == nil && (next.ListenAddress != cfg.ListenAddress || !reflect.DeepEqual(next.ProxyPeers, cfg.ProxyPeers)) {
				err = errors.New("CoreDNS listener and proxy peers require a process rollout")
			}
			if err == nil {
				err = p.Replace(next)
			}
			if err != nil {
				log.Printf("CoreDNS retains accepted state: %v", err)
				previous = data
				continue
			}
			previous = data
		}
	}
}
