// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"encoding/json"
	"sort"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func fitSnapshot(snapshot model.ServingSnapshot, prior model.ServingSnapshot, maxBindings, maxBytes int) (model.ServingSnapshot, map[string]bool) {
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
	snapshot.Bindings = nil
	rejected := map[string]bool{}
	fits := func(binding model.Binding) bool {
		if len(snapshot.Bindings) >= maxBindings {
			return false
		}
		candidate := snapshot
		candidate.Bindings = append(append([]model.Binding(nil), snapshot.Bindings...), binding)
		envelope, err := model.NewEnvelope(model.KindServingSnapshot, "dns-serving-"+model.OpaqueToken(candidate.Region+"/"+candidate.Shard)+"-e18446744073709551615-r18446744073709551615", candidate.Region, candidate.Shard, "00000000-0000-0000-0000-000000000000", candidate.ConfigurationEpoch, candidate.ConfigurationRevision, time.Now(), candidate)
		if err != nil {
			return false
		}
		payload, err := json.Marshal(envelope)
		return err == nil && len(payload) <= maxBytes
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
			binding.ConfigurationPending = true
			binding.Zones = nil
			if fits(binding) {
				snapshot.Bindings = append(snapshot.Bindings, binding)
			}
		}
	}
	sort.Slice(snapshot.Bindings, func(i, j int) bool { return snapshot.Bindings[i].BindingUID < snapshot.Bindings[j].BindingUID })
	return snapshot, rejected
}

func safeExpansion(old, candidate model.Binding) bool {
	if old.Tombstone || candidate.Tombstone || old.ConfigurationPending || candidate.ConfigurationPending || old.Authorization != candidate.Authorization || old.ProjectUID != candidate.ProjectUID || old.ContextUID != candidate.ContextUID || old.ConsumerAddress != candidate.ConsumerAddress || old.ClusterAddress != candidate.ClusterAddress {
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
