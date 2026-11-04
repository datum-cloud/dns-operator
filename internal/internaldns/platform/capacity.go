// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// envelopeSizingTime encodes at the widest RFC 3339 timestamp, so a sized
// envelope is never shorter than the one published.
var envelopeSizingTime = time.Date(2000, 1, 1, 0, 0, 0, 999999999, time.UTC)

func fitSnapshot(snapshot model.ServingSnapshot, prior model.ServingSnapshot, maxBindings, maxBytes int) (model.ServingSnapshot, map[string]bool, error) {
	previous := map[string]model.Binding{}
	for _, binding := range prior.Bindings {
		previous[binding.BindingUID] = binding
	}
	candidates := append([]model.Binding(nil), snapshot.Bindings...)
	sort.Slice(candidates, func(i, j int) bool {
		_, first := previous[candidates[i].BindingUID]
		_, second := previous[candidates[j].BindingUID]
		if first != second {
			return first
		}
		return candidates[i].BindingUID < candidates[j].BindingUID
	})
	snapshot.Bindings = []model.Binding{}
	rejected := map[string]bool{}
	envelope, err := model.NewEnvelope(model.KindServingSnapshot, "dns-serving-"+model.OpaqueToken(snapshot.Region+"/"+snapshot.Shard)+"-e18446744073709551615-r18446744073709551615", snapshot.Region, snapshot.Shard, "00000000-0000-0000-0000-000000000000", snapshot.ConfigurationEpoch, snapshot.ConfigurationRevision, envelopeSizingTime, snapshot)
	if err != nil {
		return snapshot, rejected, err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return snapshot, rejected, err
	}
	used := len(payload)
	// Each bound zone later adds its highest publication requirement to the
	// snapshot, so fitting charges for it as it will be encoded.
	required := map[string]model.PublicationRequirement{}
	encoded := func(r model.PublicationRequirement) int {
		data, _ := json.Marshal(r)
		return len(data)
	}
	fits := func(binding model.Binding) bool {
		if len(snapshot.Bindings) >= maxBindings {
			return false
		}
		data, err := json.Marshal(binding)
		if err != nil {
			return false
		}
		size := len(data)
		if len(snapshot.Bindings) > 0 {
			size++
		}
		next := map[string]model.PublicationRequirement{}
		for _, zone := range binding.Zones {
			if zone.Pending {
				continue
			}
			requirement := model.PublicationRequirement{ZoneUID: zone.ZoneUID, Epoch: zone.RequiredPublicationEpoch, Revision: zone.RequiredPublicationRevision}
			old, ok := next[zone.ZoneUID]
			if !ok {
				old, ok = required[zone.ZoneUID]
			}
			switch {
			case !ok:
				if len(required) == 0 && len(next) == 0 {
					size += len(`,"requiredPublications":[]`)
				} else {
					size++
				}
				size += encoded(requirement)
			case model.Compare(requirement.Epoch, requirement.Revision, old.Epoch, old.Revision) > 0:
				if grown := encoded(requirement) - encoded(old); grown > 0 {
					size += grown
				}
			default:
				continue
			}
			next[zone.ZoneUID] = requirement
		}
		if used+size > maxBytes {
			return false
		}
		used += size
		for uid, requirement := range next {
			required[uid] = requirement
		}
		return true
	}
	for _, binding := range candidates {
		if fits(binding) {
			snapshot.Bindings = append(snapshot.Bindings, binding)
			continue
		}
		rejected[binding.BindingUID] = true
		if old, ok := previous[binding.BindingUID]; ok {
			if safeExpansion(old, binding) && fits(old) {
				snapshot.Bindings = append(snapshot.Bindings, old)
				continue
			}
			for _, candidate := range candidates {
				if _, established := previous[candidate.BindingUID]; !established {
					rejected[candidate.BindingUID] = true
				}
			}
			return prior, rejected, fmt.Errorf("capacity cannot preserve established binding %s", binding.BindingUID)
		}
	}
	sort.Slice(snapshot.Bindings, func(i, j int) bool { return snapshot.Bindings[i].BindingUID < snapshot.Bindings[j].BindingUID })
	return snapshot, rejected, nil
}

func safeExpansion(old, candidate model.Binding) bool {
	if old.Tombstone || candidate.Tombstone || old.ConfigurationPending || candidate.ConfigurationPending || old.Version != candidate.Version || old.ProjectUID != candidate.ProjectUID || old.ContextUID != candidate.ContextUID || old.ConsumerAddress != candidate.ConsumerAddress || old.ClusterAddress != candidate.ClusterAddress {
		return false
	}
	for _, zone := range old.Zones {
		found := false
		for _, next := range candidate.Zones {
			if zone.ZoneUID == next.ZoneUID && zone.Apex == next.Apex && !next.Pending {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (p *Planner) reportCapacity(ctx context.Context, bindings []dnsv1alpha1.DNSResolverBinding, rejected map[string]bool, now time.Time) error {
	var failures []error
	for i := range bindings {
		binding := &bindings[i]
		if binding.Spec.Placement.Region != p.Config.Region || binding.Spec.Placement.Shard != p.Config.Shard {
			continue
		}
		base := binding.DeepCopy()
		condition(&binding.Status.Conditions, "CapacityAvailable", !rejected[string(binding.UID)], "ShardCapacity", binding.Generation, now)
		if !jsonEqualStatus(base.Status, binding.Status) {
			if err := p.Client.Status().Patch(ctx, binding, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}
