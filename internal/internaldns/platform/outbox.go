// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type BootstrapWriter interface {
	Put(context.Context, string, model.Envelope) (uint64, error)
}

// Outbox retries durable intent, including ambiguous publish outcomes, using a
// stable event ID. A NATS acknowledgement is transport durability, never serving
// readiness. Activation is fenced against the committed ownership pointer.
type Outbox struct {
	Client    client.Client
	Namespace string
	Region    string
	Publisher transport.ExportPublisher
	Bootstrap BootstrapWriter
	Now       func() time.Time
	// PendingOnly requires the CRD status.state selectable field (Kubernetes 1.32+).
	PendingOnly bool
}

func (o *Outbox) Step(ctx context.Context) error {
	if o.Client == nil || o.Publisher == nil || o.Namespace == "" {
		return fmt.Errorf("outbox client, publisher and namespace are required")
	}
	var list dnsv1alpha1.DNSTransportOutboxList
	opts := []client.ListOption{client.InNamespace(o.Namespace)}
	if o.Region != "" {
		opts = append(opts, client.MatchingLabels{"internal-dns.miloapis.com/region": model.SafeToken(o.Region)})
	}
	if o.PendingOnly {
		opts = append(opts, client.MatchingFieldsSelector{Selector: fields.AndSelectors(fields.OneTermNotEqualSelector("status.state", dnsValueAcknowledged), fields.OneTermNotEqualSelector("status.state", dnsValueSuperseded))})
	}
	if err := o.Client.List(ctx, &list, opts...); err != nil {
		return err
	}
	sort.Slice(list.Items, func(i, j int) bool {
		a, b := list.Items[i], list.Items[j]
		if a.Spec.Activation != b.Spec.Activation {
			return !a.Spec.Activation
		}
		return a.Name < b.Name
	})
	states := map[string]string{}
	for _, out := range list.Items {
		states[out.Name] = out.Status.State
	}
	var first error
	for i := range list.Items {
		out := &list.Items[i]
		if out.Status.State == dnsValueAcknowledged || out.Status.State == dnsValueSuperseded {
			continue
		}
		var env model.Envelope
		if model.Hash(out.Spec.Payload) != out.Spec.PayloadHash {
			if first == nil {
				first = fmt.Errorf("outbox %s payload hash mismatch", out.Name)
			}
			continue
		}
		if err := json.Unmarshal(out.Spec.Payload, &env); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if err := env.Validate(); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if env.ResourceUID != string(out.Spec.ResourceUID) || env.Epoch != uint64(out.Spec.WriterEpoch) || env.Revision != uint64(out.Spec.Revision) {
			if first == nil {
				first = fmt.Errorf("outbox envelope identity mismatch")
			}
			continue
		}
		if out.Spec.Subject != expectedSubject(env) {
			if first == nil {
				first = fmt.Errorf("outbox %s subject does not match its payload", out.Name)
			}
			continue
		}
		committed, err := o.committed(ctx, out, env)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if !committed {
			older, oldErr := o.superseded(ctx, out, env)
			if oldErr != nil && first == nil {
				first = oldErr
			}
			if older {
				base := out.DeepCopy()
				out.Status.State = dnsValueSuperseded
				if err := o.Client.Status().Patch(ctx, out, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil && first == nil {
					first = err
				}
				states[out.Name] = dnsValueSuperseded
			}
			continue
		}
		ready := true
		for _, dep := range out.Spec.DependsOn {
			ok, depErr := o.dependencyReady(ctx, env, dep, states)
			if depErr != nil && first == nil {
				first = depErr
			}
			if depErr != nil || !ok {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		ack, err := o.Publisher.Publish(ctx, out.Spec.Subject, env)
		if err == nil && o.Bootstrap != nil {
			key, keyErr := model.SnapshotKey(env)
			if keyErr != nil {
				err = keyErr
			} else {
				_, err = o.Bootstrap.Put(ctx, key, env)
			}
		}
		// Refetch before a status update to preserve concurrent acknowledgement
		// changes and resourceVersion fencing; an old object UID cannot ACK a new one.
		var current dnsv1alpha1.DNSTransportOutbox
		if getErr := o.Client.Get(ctx, client.ObjectKeyFromObject(out), &current); getErr != nil {
			if first == nil {
				first = getErr
			}
			continue
		}
		if current.UID != out.UID || current.Spec.PayloadHash != out.Spec.PayloadHash {
			if first == nil {
				first = fmt.Errorf("outbox lifetime changed during publication")
			}
			continue
		}
		base := current.DeepCopy()
		current.Status.Attempts++
		if err != nil {
			current.Status.State = dnsValuePending
			current.Status.LastError = err.Error()
			if first == nil {
				first = err
			}
		} else {
			now := time.Now().UTC()
			if o.Now != nil {
				now = o.Now().UTC()
			}
			at := metav1.NewTime(now)
			current.Status.State = dnsValueAcknowledged
			current.Status.AcknowledgedAt = &at
			current.Status.StreamSequence = ack.Sequence
			current.Status.LastError = ""
			states[out.Name] = dnsValueAcknowledged
		}
		if patchErr := o.Client.Status().Patch(ctx, &current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); patchErr != nil {
			if first == nil {
				first = patchErr
			}
			continue
		}
		if err == nil && env.Kind == model.KindPublicationManifest {
			var m dnsv1alpha1.DNSPublicationManifest
			if getErr := o.Client.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: out.Spec.ManifestRef.Name}, &m); getErr == nil {
				base := m.DeepCopy()
				m.Status.ExportState = dnsValueAcknowledged
				m.Status.StreamSequence = ack.Sequence
				m.Status.ExportedAt = current.Status.AcknowledgedAt
				if patchErr := o.Client.Status().Patch(ctx, &m, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); patchErr != nil && first == nil {
					first = patchErr
				}
			}
		}
	}
	return first
}

func (o *Outbox) committed(ctx context.Context, out *dnsv1alpha1.DNSTransportOutbox, env model.Envelope) (bool, error) {
	if env.Kind == model.KindServingSnapshot {
		name := out.Labels[shardOwnerLabel]
		if name == "" {
			return false, fmt.Errorf("serving outbox lacks its ownership record")
		}
		var cm corev1.ConfigMap
		if err := o.Client.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: name}, &cm); err != nil {
			return false, err
		}
		var s shardState
		if err := json.Unmarshal([]byte(cm.Data["state"]), &s); err != nil {
			return false, err
		}
		return cm.UID == out.Spec.ResourceUID && s.Epoch == env.Epoch && s.ActiveOutbox == out.Name, nil
	}
	if out.Spec.ManifestRef.Name == "" {
		return false, fmt.Errorf("publication outbox lacks manifest reference")
	}
	var m dnsv1alpha1.DNSPublicationManifest
	if err := o.Client.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: out.Spec.ManifestRef.Name}, &m); apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var own dnsv1alpha1.DNSPublicationOwnership
	if err := o.Client.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: "owner-" + model.OpaqueToken(string(m.Spec.ZoneRef.UID))[:20]}, &own); apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return own.Spec.WriterEpoch == int64(env.Epoch) && own.Spec.ActiveManifestName == m.Name && m.Spec.WriterEpoch == int64(env.Epoch) && m.Spec.Revision == int64(env.Revision), nil
}

func expectedSubject(env model.Envelope) string {
	switch env.Kind {
	case model.KindServingSnapshot:
		return model.ServingSubject(env.Region, env.Shard)
	case model.KindPublicationChunk:
		return model.RecordChunkSubject(env.Region, env.Shard, env.ResourceUID)
	case model.KindPublicationManifest:
		return model.RecordManifestSubject(env.Region, env.Shard, env.ResourceUID)
	default:
		return ""
	}
}

// A serving snapshot pins a minimum publication fence, rather than a historical
// activation event. A newer committed complete manifest may satisfy it. Chunk
// dependencies within a manifest remain exact: revisions cannot be mixed.
func (o *Outbox) dependencyReady(ctx context.Context, env model.Envelope, name string, states map[string]string) (bool, error) {
	if states[name] == dnsValueAcknowledged {
		return true, nil
	}
	var dep dnsv1alpha1.DNSTransportOutbox
	if err := o.Client.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: name}, &dep); apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if dep.Status.State == dnsValueAcknowledged {
		states[name] = dep.Status.State
		return true, nil
	}
	if env.Kind != model.KindServingSnapshot || dep.Spec.ManifestRef.Name == "" {
		return false, nil
	}
	var previous dnsv1alpha1.DNSPublicationManifest
	if err := o.Client.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: dep.Spec.ManifestRef.Name}, &previous); err != nil {
		return false, err
	}
	var owner dnsv1alpha1.DNSPublicationOwnership
	if err := o.Client.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: "owner-" + model.OpaqueToken(string(previous.Spec.ZoneRef.UID))[:20]}, &owner); err != nil {
		return false, err
	}
	if owner.Spec.ActiveManifestName == "" {
		return false, nil
	}
	var current dnsv1alpha1.DNSPublicationManifest
	if err := o.Client.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: owner.Spec.ActiveManifestName}, &current); err != nil {
		return false, err
	}
	if model.Compare(uint64(current.Spec.WriterEpoch), uint64(current.Spec.Revision), uint64(previous.Spec.WriterEpoch), uint64(previous.Spec.Revision)) < 0 {
		return false, nil
	}
	var activation dnsv1alpha1.DNSTransportOutbox
	key := client.ObjectKey{Namespace: o.Namespace, Name: model.PublicationActivationName(current.Name, env.Region, env.Shard)}
	if err := o.Client.Get(ctx, key, &activation); apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return activation.Status.State == dnsValueAcknowledged, nil
}

// An abandoned staged event remains pending until a strictly newer committed
// fence proves that it can never become active. This avoids unbounded pending
// scans without deleting the immutable audit/replay artifacts.
func (o *Outbox) superseded(ctx context.Context, out *dnsv1alpha1.DNSTransportOutbox, env model.Envelope) (bool, error) {
	if env.Kind == model.KindServingSnapshot {
		var cm corev1.ConfigMap
		if err := o.Client.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: out.Labels[shardOwnerLabel]}, &cm); err != nil {
			return false, err
		}
		var owner shardState
		if err := json.Unmarshal([]byte(cm.Data["state"]), &owner); err != nil {
			return false, err
		}
		if cm.UID != out.Spec.ResourceUID {
			return true, nil
		}
		if owner.ActiveOutbox == "" {
			return false, nil
		}
		var active dnsv1alpha1.DNSTransportOutbox
		if err := o.Client.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: owner.ActiveOutbox}, &active); err != nil {
			return false, err
		}
		return model.Compare(uint64(active.Spec.WriterEpoch), uint64(active.Spec.Revision), env.Epoch, env.Revision) > 0, nil
	}
	var owner dnsv1alpha1.DNSPublicationOwnership
	if err := o.Client.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: "owner-" + model.OpaqueToken(env.ResourceUID)[:20]}, &owner); apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if owner.Spec.ActiveManifestName == "" {
		return false, nil
	}
	var active dnsv1alpha1.DNSPublicationManifest
	if err := o.Client.Get(ctx, client.ObjectKey{Namespace: o.Namespace, Name: owner.Spec.ActiveManifestName}, &active); err != nil {
		return false, err
	}
	return model.Compare(uint64(active.Spec.WriterEpoch), uint64(active.Spec.Revision), env.Epoch, env.Revision) > 0, nil
}
