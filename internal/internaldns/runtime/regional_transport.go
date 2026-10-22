// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"fmt"
	"sort"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/platform"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
)

type regionalRoute struct {
	bus       *transport.JetStream
	publisher transport.ExportPublisher
	bootstrap platform.BootstrapWriter
	store     *transport.SnapshotStore
	stream    transport.StreamConfig
}

type regionalTransport struct{ routes map[string]regionalRoute }

func connectRegional(ctx context.Context, cfg Config, regions []string) (*regionalTransport, error) {
	unique := map[string]bool{}
	for _, region := range regions {
		if region == "" {
			return nil, fmt.Errorf("transport region is required")
		}
		unique[region] = true
	}
	ordered := make([]string, 0, len(unique))
	for region := range unique {
		ordered = append(ordered, region)
	}
	sort.Strings(ordered)
	routes := &regionalTransport{routes: map[string]regionalRoute{}}
	if len(cfg.RegionalTransports) == 0 {
		if len(ordered) != 1 {
			return nil, fmt.Errorf("one region per regional transport worker is required")
		}
		natsConfig := cfg.NATS
		natsConfig.Name += "-" + model.SafeToken(ordered[0])
		bus, err := connectWithRetry(ctx, natsConfig)
		if err != nil {
			return nil, err
		}
		if cfg.EnsureStream {
			if err := bus.EnsureStream(ctx, cfg.Stream); err != nil {
				_ = bus.Close()
				return nil, err
			}
		}
		store, err := bus.SnapshotStore(ctx, cfg.Stream.SnapshotsBucket)
		if err != nil {
			_ = bus.Close()
			return nil, err
		}
		for _, region := range ordered {
			routes.routes[region] = regionalRoute{bus: bus, publisher: bus, bootstrap: store, store: store, stream: cfg.Stream}
		}
		return routes, nil
	}
	for _, region := range ordered {
		nats, stream, ensure, err := cfg.transportForRegion(region)
		if err != nil {
			routes.Close()
			return nil, err
		}
		bus, err := connectWithRetry(ctx, nats)
		if err != nil {
			routes.Close()
			return nil, err
		}
		if ensure {
			if err := bus.EnsureStream(ctx, stream); err != nil {
				_ = bus.Close()
				routes.Close()
				return nil, err
			}
		}
		store, err := bus.SnapshotStore(ctx, stream.SnapshotsBucket)
		if err != nil {
			_ = bus.Close()
			routes.Close()
			return nil, err
		}
		routes.routes[region] = regionalRoute{bus: bus, publisher: bus, bootstrap: store, store: store, stream: stream}
	}
	return routes, nil
}

func (r *regionalTransport) route(region string) (regionalRoute, error) {
	v, ok := r.routes[region]
	if !ok {
		return regionalRoute{}, fmt.Errorf("no connected transport for region %q", region)
	}
	return v, nil
}
func (r *regionalTransport) Publish(ctx context.Context, subject string, env model.Envelope) (transport.PublishAck, error) {
	v, err := r.route(env.Region)
	if err != nil {
		return transport.PublishAck{}, err
	}
	return v.publisher.Publish(ctx, subject, env)
}
func (r *regionalTransport) Put(ctx context.Context, key string, env model.Envelope) (uint64, error) {
	v, err := r.route(env.Region)
	if err != nil {
		return 0, err
	}
	return v.bootstrap.Put(ctx, key, env)
}
func (r *regionalTransport) Close() {
	seen := map[*transport.JetStream]bool{}
	for _, v := range r.routes {
		if !seen[v.bus] {
			_ = v.bus.Close()
			seen[v.bus] = true
		}
	}
}
