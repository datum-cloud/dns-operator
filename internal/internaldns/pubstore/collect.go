// SPDX-License-Identifier: AGPL-3.0-only

package pubstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	publicationRetention = 10 * time.Minute
	// CollectInterval is how often Collect is meant to run.
	CollectInterval    = time.Minute
	shardOwnerLabel    = "internal-dns.miloapis.com/shard-owner"
	outboxAcknowledged = "Acknowledged"
	outboxSuperseded   = "Superseded"
)

var _ KV = (*transport.KVBucket)(nil)

// Collect deletes publication records nothing can use again: a manifest, its
// chunks and its outboxes once a strictly newer manifest of the zone is active
// and was exported more than the retention ago, and a shard's settled serving
// outboxes strictly older than its active one. Anything a pending outbox still
// depends on is kept. It then purges deletion markers older than the retention.
func (s *PublicationStore) Collect(ctx context.Context) error {
	select {
	case <-s.synced:
	case <-ctx.Done():
		return ctx.Err()
	}
	now := s.now().UTC()
	owners := map[types.UID]*dnsv1.DNSPublicationOwnership{}
	manifests := map[string]*dnsv1.DNSPublicationManifest{}
	var outboxes []*dnsv1.DNSTransportOutbox
	s.mu.RLock()
	for _, entry := range s.index {
		switch o := entry.obj.(type) {
		case *dnsv1.DNSPublicationOwnership:
			owners[o.Spec.ZoneUID] = o
		case *dnsv1.DNSPublicationManifest:
			manifests[o.Name] = o
		case *dnsv1.DNSTransportOutbox:
			outboxes = append(outboxes, o)
		}
	}
	s.mu.RUnlock()

	protected := map[string]bool{}
	byManifest := map[string][]*dnsv1.DNSTransportOutbox{}
	for _, out := range outboxes {
		if !settled(out) {
			for _, dep := range out.Spec.DependsOn {
				protected[dep] = true
			}
		}
		if out.Spec.ManifestRef.Name != "" {
			byManifest[out.Spec.ManifestRef.Name] = append(byManifest[out.Spec.ManifestRef.Name], out)
		}
	}
	var errs []error
	remove := func(obj client.Object) bool {
		if err := s.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			errs = append(errs, err)
			return false
		}
		return true
	}
	byZone := map[types.UID][]*dnsv1.DNSPublicationManifest{}
	for _, m := range manifests {
		byZone[m.Spec.ZoneRef.UID] = append(byZone[m.Spec.ZoneRef.UID], m)
	}
	for name, m := range manifests {
		own := owners[m.Spec.ZoneRef.UID]
		if own == nil || own.Spec.ActiveManifestName == name {
			continue
		}
		active := manifests[own.Spec.ActiveManifestName]
		if active == nil || model.Compare(uint64(active.Spec.WriterEpoch), uint64(active.Spec.Revision), uint64(m.Spec.WriterEpoch), uint64(m.Spec.Revision)) <= 0 {
			continue
		}
		superseded, ok := supersededAt(m, byZone[m.Spec.ZoneRef.UID])
		if !ok || now.Sub(superseded) < publicationRetention {
			continue
		}
		held := false
		for _, out := range byManifest[name] {
			held = held || protected[out.Name]
		}
		if held {
			continue
		}
		complete := true
		for _, out := range byManifest[name] {
			complete = remove(out) && complete
		}
		for _, ref := range m.Spec.Chunks {
			complete = remove(&dnsv1.DNSPublicationChunk{ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: m.Namespace}}) && complete
		}
		if complete {
			remove(m)
		}
	}
	for _, out := range outboxes {
		if out.Labels[shardOwnerLabel] == "" || !settled(out) || protected[out.Name] || now.Sub(out.CreationTimestamp.Time) < publicationRetention {
			continue
		}
		older, err := s.olderThanActiveServing(ctx, out)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if older {
			remove(out)
		}
	}
	for _, name := range s.bucketNames() {
		if purger, ok := s.buckets[name].(interface {
			PurgeDeletes(context.Context, time.Duration) error
		}); ok {
			if err := purger.PurgeDeletes(ctx, publicationRetention); err != nil {
				errs = append(errs, fmt.Errorf("purge deletion markers in bucket %q: %w", name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// supersededAt is when a newer publication of the zone was first exported,
// which only a committed publication ever is. Retention runs from then, not
// from when m itself was generated.
func supersededAt(m *dnsv1.DNSPublicationManifest, zone []*dnsv1.DNSPublicationManifest) (time.Time, bool) {
	var at time.Time
	for _, newer := range zone {
		if newer.Status.ExportedAt == nil || model.Compare(uint64(newer.Spec.WriterEpoch), uint64(newer.Spec.Revision), uint64(m.Spec.WriterEpoch), uint64(m.Spec.Revision)) <= 0 {
			continue
		}
		if at.IsZero() || newer.Status.ExportedAt.Time.Before(at) {
			at = newer.Status.ExportedAt.Time
		}
	}
	return at, !at.IsZero()
}

// olderThanActiveServing reads the shard's lease and active serving outbox
// from the bucket, not the index, and reports whether out is strictly older.
func (s *PublicationStore) olderThanActiveServing(ctx context.Context, out *dnsv1.DNSTransportOutbox) (bool, error) {
	var cm corev1.ConfigMap
	if err := s.Get(ctx, client.ObjectKey{Namespace: out.Namespace, Name: out.Labels[shardOwnerLabel]}, &cm); apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var state struct {
		ActiveOutbox string `json:"activeOutbox"`
	}
	if err := json.Unmarshal([]byte(cm.Data["state"]), &state); err != nil || state.ActiveOutbox == "" || state.ActiveOutbox == out.Name {
		return false, nil
	}
	var active dnsv1.DNSTransportOutbox
	if err := s.Get(ctx, client.ObjectKey{Namespace: out.Namespace, Name: state.ActiveOutbox}, &active); apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return active.Spec.ResourceUID == out.Spec.ResourceUID && model.Compare(uint64(active.Spec.WriterEpoch), uint64(active.Spec.Revision), uint64(out.Spec.WriterEpoch), uint64(out.Spec.Revision)) > 0, nil
}

func settled(out *dnsv1.DNSTransportOutbox) bool {
	return out.Status.State == outboxAcknowledged || out.Status.State == outboxSuperseded
}
