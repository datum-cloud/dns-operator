// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/feed"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// FeedConfig carries the inputs this region writes itself to the other
// regions over NATS, and up to the federation hub.
type FeedConfig struct {
	// Bucket is this region's input bucket on its own broker.
	Bucket   string `json:"bucket"`
	Replicas int    `json:"replicas,omitempty"`
	// HubKubeconfig, when set, reaches the federation hub, which receives a
	// copy of every input this region writes so it reaches the project API.
	HubKubeconfig string `json:"hubKubeconfig,omitempty"`
	// Peers are the other regions whose input buckets this region's broker
	// mirrors.
	Peers []FeedPeer `json:"peers,omitempty"`
}

// FeedPeer is another region's input bucket, mirrored onto this region's
// broker from that region's JetStream domain.
type FeedPeer struct {
	Region string `json:"region"`
	Domain string `json:"domain"`
	Bucket string `json:"bucket"`
	// Mirror is the local bucket that holds the copy.
	Mirror string `json:"mirror"`
}

func (c Config) validateFeed() error {
	f := c.Feed
	if f == nil {
		return nil
	}
	if len(c.Regions) != 1 {
		return errors.New("feed requires exactly one region in regions: the region that writes the feed")
	}
	if !validBucketName(f.Bucket) || f.Replicas < 0 {
		return errors.New("feed requires a bucket name of letters, digits, - and _, and non-negative replicas")
	}
	seen := map[string]bool{}
	mirrors := map[string]bool{f.Bucket: true}
	for i, p := range f.Peers {
		if p.Region == "" || p.Region == c.Regions[0] || seen[p.Region] || p.Domain == "" || !validBucketName(p.Bucket) || !validBucketName(p.Mirror) || mirrors[p.Mirror] {
			return fmt.Errorf("feed.peers[%d] needs another region listed once, its JetStream domain, its bucket and a local mirror bucket of its own", i)
		}
		seen[p.Region] = true
		mirrors[p.Mirror] = true
	}
	return nil
}

// runFeed publishes this region's own inputs and applies every peer's. It
// keeps retrying the broker so that an unreachable broker never stops the
// rest of the control plane.
func runFeed(ctx context.Context, cfg Config, scheme *k8sruntime.Scheme, local *rest.Config, live *loopLiveness, errCh chan<- error) {
	region := cfg.Regions[0]
	log := ctrl.LoggerFrom(ctx).WithName("internal-dns-feed").WithValues("region", region)
	natsConfig, _, _, err := cfg.transportForRegion(region)
	if err != nil {
		errCh <- err
		return
	}
	natsConfig.Name += "-feed"
	bus, err := connectWithRetry(ctx, natsConfig)
	if err != nil {
		if ctx.Err() == nil {
			errCh <- err
		}
		return
	}
	defer func() { _ = bus.Close() }()
	var bucket *bucketHandle
	for ctx.Err() == nil {
		kv, err := bus.KeyValueBucket(ctx, cfg.Feed.Bucket, cfg.Feed.Replicas, nil)
		if err == nil {
			bucket = &bucketHandle{kv}
			break
		}
		log.Error(err, "open the region's input bucket; retrying")
		if !waitForRetry(ctx) {
			return
		}
	}
	if bucket == nil {
		return
	}
	var hub client.Client
	if cfg.Feed.HubKubeconfig != "" {
		hubConfig, err := restConfig(cfg.Feed.HubKubeconfig)
		if err != nil {
			errCh <- fmt.Errorf("load feed hub kubeconfig: %w", err)
			return
		}
		direct, err := client.New(hubConfig, client.Options{Scheme: scheme})
		if err != nil {
			errCh <- fmt.Errorf("create feed hub client: %w", err)
			return
		}
		hub = &deadlineClient{Client: direct, Timeout: 10 * time.Second}
	}
	localClient, err := client.New(local, client.Options{Scheme: scheme})
	if err != nil {
		errCh <- err
		return
	}
	publisher := &feed.Publisher{Region: region, Local: localClient, KV: bucket.kv, Hub: hub}
	if err := startPublisher(ctx, local, scheme, region, publisher); err != nil {
		errCh <- err
		return
	}
	go retryLoop(ctx, time.Minute, loopID{Loop: "feed resync", Region: region}, live, publisher.Resync, errCh)
	for _, peer := range cfg.Feed.Peers {
		go func() {
			for ctx.Err() == nil {
				mirror, err := bus.MirrorKeyValueBucket(ctx, peer.Mirror, peer.Bucket, peer.Domain, cfg.Feed.Replicas)
				if err == nil {
					applier := &feed.Applier{Region: peer.Region, Local: localClient, Mirror: mirror, CaughtUp: mirror.MirrorCaughtUp}
					err = applier.Run(ctx)
				}
				if err != nil && ctx.Err() == nil {
					log.Error(err, "apply another region's inputs; retrying", "peer", peer.Region)
				}
				if !waitForRetry(ctx) {
					return
				}
			}
		}()
	}
	<-ctx.Done()
}

type bucketHandle struct{ kv feed.KV }

// startPublisher publishes each of this region's own inputs as it changes.
func startPublisher(ctx context.Context, local *rest.Config, scheme *k8sruntime.Scheme, region string, p *feed.Publisher) error {
	selector := labels.SelectorFromSet(labels.Set{feed.OriginLabel: region})
	byObject := map[client.Object]cache.ByObject{}
	for _, gvk := range feed.Kinds {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		byObject[obj] = cache.ByObject{Label: selector}
	}
	mgr, err := ctrl.NewManager(local, ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cache.Options{ByObject: byObject},
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	if err != nil {
		return fmt.Errorf("create feed manager: %w", err)
	}
	for _, gvk := range feed.Kinds {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := builder.ControllerManagedBy(mgr).Named("feed-" + model.SafeToken(gvk.Kind)).For(obj).Complete(reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
			return reconcile.Result{}, p.Sync(ctx, gvk, req.NamespacedName)
		})); err != nil {
			return err
		}
	}
	go func() {
		if err := mgr.Start(ctx); err != nil && ctx.Err() == nil {
			ctrl.LoggerFrom(ctx).Error(err, "feed publisher stopped")
		}
	}()
	return nil
}
