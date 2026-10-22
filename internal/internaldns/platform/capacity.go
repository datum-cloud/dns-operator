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
	envelope, err := model.NewEnvelope(model.KindServingSnapshot, "dns-serving-"+model.OpaqueToken(snapshot.Region+"/"+snapshot.Shard)+"-e18446744073709551615-r18446744073709551615", snapshot.Region, snapshot.Shard, "00000000-0000-0000-0000-000000000000", snapshot.ConfigurationEpoch, snapshot.ConfigurationRevision, time.Now(), snapshot)
	if err != nil {
		return snapshot, rejected, err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return snapshot, rejected, err
	}
	used := len(payload)
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
		if used+size > maxBytes {
			return false
		}
		used += size
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
