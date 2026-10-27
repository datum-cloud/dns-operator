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
	"github.com/coredns/coredns/plugin/metrics"
	"github.com/miekg/dns"
)

// CoreDNS refuses every non-IN query before the plugin chain unless a listed
// plugin opts in. The plugin answers only the CH readiness canary and refuses
// every other non-IN query before it selects a context.
func init() { dnsserver.EnableChaos[(&ContextPlugin{}).Name()] = struct{}{} }

func Run(ctx context.Context, path, metricsAddress string) error {
	return Process{MetricsAddress: metricsAddress}.Run(ctx, path)
}

type Process struct {
	MetricsAddress  string
	IdentityKeyFile *string
}

func (pr Process) Run(ctx context.Context, path string) error {
	metricsAddress := pr.MetricsAddress
	previous, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	cfg, err := Decode(previous)
	if err != nil {
		return err
	}
	if pr.IdentityKeyFile != nil {
		cfg.IdentityKeyFile = *pr.IdentityKeyFile
	}
	p := &ContextPlugin{}
	if cfg.IdentityKeyFile != "" {
		if p.identityKey, err = LoadIdentityKey(cfg.IdentityKeyFile); err != nil {
			return fmt.Errorf("CoreDNS identity key: %w", err)
		}
	}
	if err := p.Replace(cfg); err != nil {
		return err
	}
	defer p.Close()
	host, port, _ := net.SplitHostPort(cfg.ListenAddress)
	chain := []plugin.Plugin{func(plugin.Handler) plugin.Handler { return p }}
	var metricsDone <-chan error
	if metricsAddress != "" {
		queries := metrics.New(metricsAddress)
		queries.AddZone(".")
		chain = append([]plugin.Plugin{func(next plugin.Handler) plugin.Handler { queries.Next = next; return queries }}, chain...)
		metricsCtx, stopMetrics := context.WithCancel(ctx)
		defer stopMetrics()
		if metricsDone, err = serveMetrics(metricsCtx, metricsAddress, p); err != nil {
			log.Printf("CoreDNS serves without metrics: %v", err)
		}
	}
	server, err := dnsserver.NewServer("dns://"+cfg.ListenAddress, []*dnsserver.Config{{
		Zone: ".", Port: port, Transport: "dns", ListenHosts: []string{host},
		Plugin: chain,
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
	listener := &proxyListener{Listener: tcp, peers: peers, plain: p.identityKey != nil}
	defer func() { _ = listener.Close() }()
	var udp *dns.Server
	if p.identityKey != nil {
		packet, err := listenUDP(cfg.ListenAddress, cfg.UDPReadBufferBytes)
		if err != nil {
			return fmt.Errorf("CoreDNS UDP listener: %w", err)
		}
		defer func() { _ = packet.Close() }()
		if size, err := udpReadBufferSize(packet); err != nil {
			log.Printf("CoreDNS UDP receive buffer inspection: %v", err)
		} else if size > 0 {
			log.Printf("CoreDNS UDP receive buffer requested=%d effective=%d bytes", cfg.UDPReadBufferBytes, size)
		}
		if metricsAddress != "" {
			cleanup, err := observeUDPSocket(packet)
			if err != nil {
				log.Printf("CoreDNS serves without UDP socket metrics: %v", err)
			} else {
				defer cleanup()
			}
		}
		udp = keyedUDPServer(packet, peers, server)
	}
	stop := func() error {
		if udp != nil {
			shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = udp.ShutdownContext(shutdown)
		}
		return server.Stop()
	}
	defer func() { _ = stop() }()
	done := make(chan error, 2)
	go func() { done <- server.Serve(listener) }()
	if udp != nil {
		go func() { done <- udp.ActivateAndServe() }()
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return stop()
		case err := <-done:
			_ = stop()
			return fmt.Errorf("CoreDNS listener stopped: %w", errors.Join(err, errors.New("unexpected listener exit")))
		case err := <-metricsDone:
			metricsDone = nil
			if ctx.Err() == nil {
				log.Printf("CoreDNS serves without metrics: %v", errors.Join(err, errors.New("metrics listener stopped")))
			}
		case <-ticker.C:
			data, err := os.ReadFile(path)
			if err != nil || bytes.Equal(previous, data) {
				continue
			}
			next, err := Decode(data)
			if err == nil && pr.IdentityKeyFile != nil {
				next.IdentityKeyFile = *pr.IdentityKeyFile
			}
			if err == nil && (next.ListenAddress != cfg.ListenAddress || !reflect.DeepEqual(next.ProxyPeers, cfg.ProxyPeers) || next.IdentityKeyFile != cfg.IdentityKeyFile || next.UDPReadBufferBytes != cfg.UDPReadBufferBytes) {
				_ = stop()
				return errors.New("CoreDNS restarts to apply a new listener, proxy peers, identity key, or UDP read buffer")
			}
			if err == nil {
				err = p.Replace(next)
			} else {
				snapshotReloads.WithLabelValues("rejected").Inc()
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
