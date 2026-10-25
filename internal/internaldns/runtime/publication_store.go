// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"fmt"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/pubstore"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// openPublicationStore opens the bucket on every region's broker, so each
// region's records live with that region's broker. Regions that share one
// broker share its bucket.
func openPublicationStore(ctx context.Context, cfg Config, api client.Client) (*pubstore.PublicationStore, func(), error) {
	regions := []string{cfg.PublicationStore.Region}
	if len(cfg.RegionalTransports) > 0 {
		regions = regions[:0]
		for _, regional := range cfg.RegionalTransports {
			regions = append(regions, regional.Region)
		}
	}
	var buses []*transport.JetStream
	closeAll := func() {
		for _, bus := range buses {
			_ = bus.Close()
		}
	}
	buckets := map[string]pubstore.KV{}
	for _, region := range regions {
		natsConfig, _, _, err := cfg.transportForRegion(region)
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		natsConfig.Name += "-publication-store"
		bus, err := connectWithRetry(ctx, natsConfig)
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		buses = append(buses, bus)
		bucket, err := bus.KeyValueBucket(ctx, cfg.PublicationStore.Bucket, cfg.PublicationStore.Replicas, pubstore.WatchFilter())
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		buckets[model.SafeToken(region)] = pubstore.Instrumented(bucket)
	}
	store := pubstore.NewRegional(api, model.SafeToken(cfg.PublicationStore.Region), buckets)
	if err := store.Start(ctx); err != nil {
		closeAll()
		return nil, nil, fmt.Errorf("load publication store: %w", err)
	}
	return store, closeAll, nil
}
