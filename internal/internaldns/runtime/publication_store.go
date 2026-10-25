// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/pubstore"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// openPublicationStore opens the bucket on every region's broker, so each
// region's records live with that region's broker. Regions that share one
// broker, by URL, share its bucket.
func openPublicationStore(ctx context.Context, cfg Config, api client.Client) (*pubstore.PublicationStore, func(), error) {
	regions := []string{cfg.PublicationStore.Region}
	if len(cfg.RegionalTransports) > 0 {
		regions = regions[:0]
		for _, regional := range cfg.RegionalTransports {
			regions = append(regions, regional.Region)
		}
	}
	home, _, _, err := cfg.transportForRegion(cfg.PublicationStore.Region)
	if err != nil {
		return nil, nil, err
	}
	storeCtx, cancel := context.WithCancel(ctx)
	buckets := map[string]pubstore.KV{}
	byRegion := map[string]string{}
	var opened []*lazyBucket
	for _, region := range regions {
		natsConfig, _, _, err := cfg.transportForRegion(region)
		if err != nil {
			cancel()
			return nil, nil, err
		}
		if _, ok := buckets[natsConfig.URL]; !ok {
			natsConfig.Name += "-publication-store"
			lazy := &lazyBucket{url: natsConfig.URL}
			opened = append(opened, lazy)
			buckets[natsConfig.URL] = lazy
			if natsConfig.URL == home.URL {
				if err := lazy.open(storeCtx, natsConfig, cfg.PublicationStore); err != nil {
					cancel()
					return nil, nil, err
				}
			} else {
				go func() {
					if err := lazy.open(storeCtx, natsConfig, cfg.PublicationStore); err != nil && storeCtx.Err() == nil {
						ctrl.LoggerFrom(ctx).Error(err, "regional publication store unavailable", "url", natsConfig.URL)
					}
				}()
			}
		}
		byRegion[model.SafeToken(region)] = natsConfig.URL
	}
	closeAll := func() {
		cancel()
		for _, lazy := range opened {
			lazy.close()
		}
	}
	store := pubstore.NewRegional(api, home.URL, buckets, byRegion)
	if err := store.Start(storeCtx); err != nil {
		closeAll()
		return nil, nil, fmt.Errorf("load publication store: %w", err)
	}
	return store, closeAll, nil
}

var errBucketUnavailable = errors.New("publication store bucket is not connected yet")

// lazyBucket is a regional bucket whose broker may come up after the control
// plane; until it does, every request to it fails and the rest are unaffected.
type lazyBucket struct {
	url string
	mu  sync.RWMutex
	kv  pubstore.KV
	bus *transport.JetStream
}

func (b *lazyBucket) open(ctx context.Context, natsConfig transport.Config, store *PublicationStoreConfig) error {
	bus, err := connectWithRetry(ctx, natsConfig)
	if err != nil {
		return err
	}
	for {
		bucket, err := bus.KeyValueBucket(ctx, store.Bucket, store.Replicas, pubstore.WatchFilter())
		if err == nil {
			b.mu.Lock()
			b.kv, b.bus = pubstore.Instrumented(bucket), bus
			b.mu.Unlock()
			return nil
		}
		if ctx.Err() != nil {
			_ = bus.Close()
			return err
		}
		ctrl.LoggerFrom(ctx).Error(err, "open publication store bucket; retrying", "url", b.url)
		if !waitForRetry(ctx) {
			_ = bus.Close()
			return ctx.Err()
		}
	}
}

func (b *lazyBucket) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bus != nil {
		_ = b.bus.Close()
	}
}

func (b *lazyBucket) get() (pubstore.KV, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.kv == nil {
		return nil, errBucketUnavailable
	}
	return b.kv, nil
}

func (b *lazyBucket) Get(ctx context.Context, key string) ([]byte, uint64, error) {
	kv, err := b.get()
	if err != nil {
		return nil, 0, err
	}
	return kv.Get(ctx, key)
}

func (b *lazyBucket) Create(ctx context.Context, key string, value []byte) (uint64, error) {
	kv, err := b.get()
	if err != nil {
		return 0, err
	}
	return kv.Create(ctx, key, value)
}

func (b *lazyBucket) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	kv, err := b.get()
	if err != nil {
		return 0, err
	}
	return kv.Update(ctx, key, value, revision)
}

func (b *lazyBucket) Delete(ctx context.Context, key string, revision uint64) error {
	kv, err := b.get()
	if err != nil {
		return err
	}
	return kv.Delete(ctx, key, revision)
}

func (b *lazyBucket) Watch(ctx context.Context) (<-chan *transport.KVEvent, error) {
	kv, err := b.get()
	if err != nil {
		return nil, err
	}
	return kv.Watch(ctx)
}

func (b *lazyBucket) PurgeDeletes(ctx context.Context, age time.Duration) error {
	kv, err := b.get()
	if err != nil {
		return err
	}
	if purger, ok := kv.(interface {
		PurgeDeletes(context.Context, time.Duration) error
	}); ok {
		return purger.PurgeDeletes(ctx, age)
	}
	return nil
}
