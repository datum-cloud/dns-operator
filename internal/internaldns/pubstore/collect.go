// SPDX-License-Identifier: AGPL-3.0-only

package pubstore

import (
	"context"
	"encoding/json"
	"errors"
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
// and the retention has passed, and a shard's settled serving outboxes other
// than its active one. Anything a pending outbox still depends on is kept.
func (s *PublicationStore) Collect(ctx context.Context) error {
	select {
	case <-s.synced:
	case <-ctx.Done():
		return ctx.Err()
	}
	now := s.now().UTC()
	owners := map[types.UID]*dnsv1.DNSPublicationOwnership{}
	manifests := map[string]*dnsv1.DNSPublicationManifest{}
	shards := map[string]*corev1.ConfigMap{}
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
		case *corev1.ConfigMap:
			shards[o.Name] = o
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
		if err := s.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
			return false
		}
		return true
	}
	for name, m := range manifests {
		own := owners[m.Spec.ZoneRef.UID]
		if own == nil || own.Spec.ActiveManifestName == name || now.Sub(m.Spec.GeneratedAt.Time) < publicationRetention {
			continue
		}
		active := manifests[own.Spec.ActiveManifestName]
		if active == nil || model.Compare(uint64(active.Spec.WriterEpoch), uint64(active.Spec.Revision), uint64(m.Spec.WriterEpoch), uint64(m.Spec.Revision)) <= 0 {
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
		owner := out.Labels[shardOwnerLabel]
		if owner == "" || !settled(out) || protected[out.Name] || now.Sub(out.CreationTimestamp.Time) < publicationRetention {
			continue
		}
		cm := shards[owner]
		var state struct {
			ActiveOutbox string `json:"activeOutbox"`
		}
		if cm == nil || json.Unmarshal([]byte(cm.Data["state"]), &state) != nil || state.ActiveOutbox == out.Name {
			continue
		}
		remove(out)
	}
	return errors.Join(errs...)
}

func settled(out *dnsv1.DNSTransportOutbox) bool {
	return out.Status.State == outboxAcknowledged || out.Status.State == outboxSuperseded
}
