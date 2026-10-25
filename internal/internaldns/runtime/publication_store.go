// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"fmt"

	"go.miloapis.com/dns-operator/internal/internaldns/pubstore"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func openPublicationStore(ctx context.Context, cfg Config, api client.Client) (*pubstore.PublicationStore, func(), error) {
	natsConfig, _, _, err := cfg.transportForRegion(cfg.PublicationStore.Region)
	if err != nil {
		return nil, nil, err
	}
	natsConfig.Name += "-publication-store"
	bus, err := connectWithRetry(ctx, natsConfig)
	if err != nil {
		return nil, nil, err
	}
	bucket, err := bus.KeyValueBucket(ctx, cfg.PublicationStore.Bucket, cfg.PublicationStore.Replicas, pubstore.WatchFilter())
	if err != nil {
		_ = bus.Close()
		return nil, nil, err
	}
	store := pubstore.New(api, pubstore.Instrumented(bucket))
	if err := store.Start(ctx); err != nil {
		_ = bus.Close()
		return nil, nil, fmt.Errorf("load publication store: %w", err)
	}
	return store, func() { _ = bus.Close() }, nil
}
