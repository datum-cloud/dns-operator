// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const shardOwnerLabel = "internal-dns.miloapis.com/shard-owner"

type PlannerConfig struct {
	Namespace       string
	Region          string
	Shard           string
	Identity        string
	LeaseDuration   time.Duration
	NodeBackends    []model.Backend
	ClusterBackends []model.Backend
	Members         []model.Member
	// Hard safety budgets; production values require capacity qualification.
	MaxBindings      int `json:"maxBindings,omitempty"`
	MaxSnapshotBytes int `json:"maxSnapshotBytes,omitempty"`
}

type shardState struct {
	Holder       string    `json:"holder"`
	Epoch        uint64    `json:"epoch"`
	NextRevision uint64    `json:"nextRevision"`
	LeaseUntil   time.Time `json:"leaseUntil"`
	ActiveOutbox string    `json:"activeOutbox,omitempty"`
	ContentHash  string    `json:"contentHash,omitempty"`
}

// Planner is the single fenced logical writer of a complete shard snapshot.
// Project reconcilers only create binding data; this planner merges every project
// in the shared shard so a tenant's update cannot erase another tenant's views.
type Planner struct {
	Client    client.Client
	Allocator *Allocator
	Config    PlannerConfig
	Now       func() time.Time
}

func (p *Planner) Step(ctx context.Context) error {
	c := p.Config
	if p.Client == nil || p.Allocator == nil || c.Namespace == "" || c.Region == "" || c.Shard == "" || c.Identity == "" {
		return fmt.Errorf("shared planner requires client, allocator, namespace, region, shard and holder identity")
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	lease := c.LeaseDuration
	if lease <= 0 {
		lease = 30 * time.Second
	}
	key := client.ObjectKey{Namespace: c.Namespace, Name: "dns-shard-owner-" + model.OpaqueToken(c.Region+"/"+c.Shard)}
	var owner corev1.ConfigMap
	if err := p.Client.Get(ctx, key, &owner); apierrors.IsNotFound(err) {
		state := shardState{Holder: c.Identity, Epoch: 1, NextRevision: 1, LeaseUntil: now.Add(lease)}
		b, _ := json.Marshal(state)
		owner = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}, Data: map[string]string{"state": string(b)}}
		if err := p.Client.Create(ctx, &owner); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		return nil
	} else if err != nil {
		return err
	}
	var state shardState
	if err := json.Unmarshal([]byte(owner.Data["state"]), &state); err != nil {
		return fmt.Errorf("invalid shard ownership: %w", err)
	}
	if state.Holder != c.Identity {
		if state.LeaseUntil.After(now) {
			return nil
		}
		state.Holder = c.Identity
		state.Epoch++
		state.ContentHash = ""
		state.LeaseUntil = now.Add(lease)
		b, _ := json.Marshal(state)
		owner.Data["state"] = string(b)
		if err := p.Client.Update(ctx, &owner); err != nil {
			return err
		}
	}
	var bindings dnsv1alpha1.DNSResolverBindingList
	if err := p.Client.List(ctx, &bindings, client.InNamespace(c.Namespace), client.MatchingLabels{"internal-dns.miloapis.com/region": model.SafeToken(c.Region), "internal-dns.miloapis.com/shard": model.SafeToken(c.Shard)}); err != nil {
		return err
	}
	byZone := map[string]dnsv1alpha1.DNSPublicationOwnership{}
	for _, binding := range bindings.Items {
		for _, zoneRef := range binding.Spec.Configuration.ZoneRefs {
			uid := zoneRef.UID
			if _, exists := byZone[string(uid)]; exists {
				continue
			}
			var owner dnsv1alpha1.DNSPublicationOwnership
			if err := p.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: "owner-" + model.OpaqueToken(string(uid))[:20]}, &owner); apierrors.IsNotFound(err) {
				continue
			} else if err != nil {
				return err
			}
			byZone[string(uid)] = owner
		}
	}
	snapshot := model.ServingSnapshot{Region: c.Region, Shard: c.Shard, ConfigurationEpoch: state.Epoch, ConfigurationRevision: state.NextRevision, GeneratedAt: now, Members: append([]model.Member(nil), c.Members...)}
	var prior model.ServingSnapshot
	if state.ActiveOutbox != "" {
		var out dnsv1alpha1.DNSTransportOutbox
		if err := p.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: state.ActiveOutbox}, &out); err != nil {
			return err
		}
		var e model.Envelope
		if err := json.Unmarshal(out.Spec.Payload, &e); err != nil {
			return err
		}
		if err := json.Unmarshal(e.Payload, &prior); err != nil {
			return err
		}
	}
	priorBindings := map[string]model.Binding{}
	for _, b := range prior.Bindings {
		priorBindings[b.BindingUID] = b
	}
	dependencies := map[string]bool{}
	for _, obj := range bindings.Items {
		b := obj.Spec
		if b.Placement.Region != c.Region || b.Placement.Shard != c.Shard || b.Tombstone || !obj.DeletionTimestamp.IsZero() {
			continue
		}
		if b.Configuration.Listeners.Node.Port != 53 || b.Configuration.Listeners.Regional.Port != 53 || !reflect.DeepEqual(b.Configuration.Listeners.Node.Transports, b.Configuration.Listeners.Regional.Transports) {
			return fmt.Errorf("binding %s requires matching DNS listener ports and transports", obj.Name)
		}
		view := model.Binding{BindingUID: string(obj.UID), ProjectUID: string(b.Source.ProjectUID), ContextUID: string(b.Source.ResolverContextRef.UID), BindingGeneration: uint64(b.Configuration.Generation), ConfigurationRevision: uint64(b.Configuration.Revision), ConsumerAddress: b.Configuration.Listeners.Node.Address, ClusterAddress: b.Configuration.Listeners.Regional.Address, Port: uint16(b.Configuration.Listeners.Node.Port), NodeBackends: c.NodeBackends, ClusterBackends: c.ClusterBackends, Authorization: model.Authorization{IssuerEpoch: uint64(b.Authorization.WriterEpoch), Revision: uint64(b.Authorization.Sequence), ValidUntil: b.Authorization.ValidUntil.Time}}
		for _, t := range b.Configuration.Listeners.Node.Transports {
			view.Transports = append(view.Transports, model.Transport(t))
		}
		complete := true
		for _, zoneRef := range b.Configuration.ZoneRefs {
			uid := zoneRef.UID
			o, ok := byZone[string(uid)]
			if !ok || o.Spec.ActiveManifestName == "" {
				complete = false
				break
			}
			var m dnsv1alpha1.DNSPublicationManifest
			if err := p.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: o.Spec.ActiveManifestName}, &m); err != nil {
				return err
			}
			targeted := false
			for _, target := range m.Spec.ServingTargets {
				if target.Region == c.Region && target.Shard == c.Shard {
					targeted = true
					break
				}
			}
			if m.Spec.ZoneRef.UID != uid || m.Spec.Tombstone || !targeted {
				complete = false
				break
			}
			attachment := model.ZoneAttachment{ZoneUID: string(uid), Apex: m.Spec.ZoneApex, RequiredPublicationEpoch: uint64(m.Spec.WriterEpoch), RequiredPublicationRevision: uint64(m.Spec.Revision)}
			// Record updates do not change a view's minimum activation dependency.
			if old, ok := priorBindings[view.BindingUID]; ok && old.BindingGeneration == view.BindingGeneration {
				for _, z := range old.Zones {
					if z.ZoneUID == attachment.ZoneUID && z.Apex == attachment.Apex {
						attachment.RequiredPublicationEpoch = z.RequiredPublicationEpoch
						attachment.RequiredPublicationRevision = z.RequiredPublicationRevision
					}
				}
			}
			activationName := model.PublicationActivationName(m.Name, c.Region, c.Shard)
			view.Zones = append(view.Zones, attachment)
			dependencies[activationName] = true
		}
		if !complete {
			continue
		}
		sort.Slice(view.Zones, func(i, j int) bool { return view.Zones[i].ZoneUID < view.Zones[j].ZoneUID })
		snapshot.Bindings = append(snapshot.Bindings, view)
	}
	sort.Slice(snapshot.Bindings, func(i, j int) bool { return snapshot.Bindings[i].BindingUID < snapshot.Bindings[j].BindingUID })
	sort.Slice(snapshot.Members, func(i, j int) bool { return snapshot.Members[i].MemberID < snapshot.Members[j].MemberID })
	maxBindings := c.MaxBindings
	if maxBindings <= 0 {
		maxBindings = 256
	}
	if len(snapshot.Bindings) > maxBindings {
		return fmt.Errorf("shard %s/%s exceeds binding budget: %d > %d; no partial snapshot will be exported", c.Region, c.Shard, len(snapshot.Bindings), maxBindings)
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	comparison := snapshot
	comparison.GeneratedAt = time.Time{}
	comparison.ConfigurationEpoch = 0
	comparison.ConfigurationRevision = 0
	b, _ := json.Marshal(comparison)
	hash := model.Hash(b)
	if state.ContentHash == hash {
		if state.LeaseUntil.Sub(now) > lease/2 {
			return nil
		}
		state.LeaseUntil = now.Add(lease)
		b, _ := json.Marshal(state)
		owner.Data["state"] = string(b)
		return p.Client.Update(ctx, &owner)
	}
	if !state.LeaseUntil.After(now) {
		return fmt.Errorf("shared planner lost its lease while staging snapshot")
	}
	// Reserve the revision before writing immutable artifacts. A crash after
	// staging leaves an orphan, and retry uses a fresh revision instead of trying
	// to rewrite an immutable event with a different timestamp or desired state.
	revision := state.NextRevision
	state.NextRevision++
	state.LeaseUntil = now.Add(lease)
	reserved, _ := json.Marshal(state)
	owner.Data["state"] = string(reserved)
	if err := p.Client.Update(ctx, &owner); err != nil {
		return err
	}
	name := fmt.Sprintf("dns-serving-%s-e%d-r%d", model.OpaqueToken(c.Region+"/"+c.Shard), state.Epoch, revision)
	env, err := model.NewEnvelope(model.KindServingSnapshot, name, c.Region, c.Shard, string(owner.UID), state.Epoch, revision, now, snapshot)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(env)
	maxBytes := c.MaxSnapshotBytes
	if maxBytes <= 0 {
		maxBytes = 512 * 1024
	}
	if len(payload) > maxBytes {
		return fmt.Errorf("shard serving envelope exceeds configured transport budget: %d > %d", len(payload), maxBytes)
	}
	deps := make([]string, 0, len(dependencies))
	for name := range dependencies {
		deps = append(deps, name)
	}
	sort.Strings(deps)
	out := &dnsv1alpha1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: map[string]string{shardOwnerLabel: owner.Name, "internal-dns.miloapis.com/region": model.SafeToken(c.Region), "internal-dns.miloapis.com/shard": model.SafeToken(c.Shard)}}, Spec: dnsv1alpha1.DNSTransportOutboxSpec{Subject: model.ServingSubject(c.Region, c.Shard), ResourceUID: owner.UID, WriterEpoch: int64(state.Epoch), Revision: int64(revision), PayloadHash: model.Hash(payload), Payload: payload, Activation: true, DependsOn: deps}}
	if err := p.Client.Create(ctx, out); apierrors.IsAlreadyExists(err) {
		var existing dnsv1alpha1.DNSTransportOutbox
		if err := p.Client.Get(ctx, client.ObjectKeyFromObject(out), &existing); err != nil {
			return err
		}
		if !reflect.DeepEqual(existing.Spec, out.Spec) {
			return fmt.Errorf("immutable serving revision collision")
		}
	} else if err != nil {
		return err
	}
	state.ActiveOutbox = name
	state.ContentHash = hash
	state.LeaseUntil = now.Add(lease)
	b, _ = json.Marshal(state)
	owner.Data["state"] = string(b)
	// Publication is enabled only after this resourceVersion-checked commit.
	return p.Client.Update(ctx, &owner)
}
