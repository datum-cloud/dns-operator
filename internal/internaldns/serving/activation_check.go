// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

// CheckActivation requires the latest locally accepted state to be fully
// approved. Unlike readiness, it never accepts an installation in progress.
func CheckActivation(ctx context.Context, config Config) (string, error) {
	return checkActivation(ctx, config, Readiness)
}

func checkActivation(ctx context.Context, config Config, probe func(context.Context, Config) error) (string, error) {
	before, err := currentActivation(config)
	if err != nil {
		return "", err
	}
	if err := probe(ctx, config); err != nil {
		return "", err
	}
	after, err := currentActivation(config)
	if err != nil {
		return "", err
	}
	if !equalJSON(before.Active, after.Active) || !equalJSON(before.Snapshot, after.Snapshot) || !equalJSON(before.Publications, after.Publications) {
		return "", errors.New("activation changed during validation; retry before cleanup")
	}
	data, err := json.Marshal(after.Active)
	if err != nil {
		return "", err
	}
	return model.Hash(data), nil
}

func currentActivation(config Config) (checkpoint, error) {
	state, err := (stateStore{path: filepath.Join(config.StateDir, "checkpoint.json")}).Load()
	if err != nil {
		return state, err
	}
	if activationPending(config.StateDir) {
		return state, errors.New("activation is pending")
	}
	if state.Active == nil || state.Snapshot == nil {
		return state, errors.New("no current approved activation")
	}
	active := state.Active
	if active.NodeAuthoritative != config.NodeAuthoritative || !equalJSON(active.Snapshot, *state.Snapshot) {
		return state, errors.New("approved activation is behind the accepted configuration")
	}
	if state.RenderedHash != hashFiles(active.Files) || !installedFilesMatch(active.Files) {
		return state, errors.New("installed files differ from the approved activation")
	}
	agent := Agent{cfg: config, state: state}
	render := config.Render
	render.ReadyMembers = map[string]map[string]bool{}
	render.ReadyZoneMembers = map[string]map[string]map[string]bool{}
	enabled := map[string]bool{}
	for _, binding := range state.Snapshot.Bindings {
		if binding.Tombstone {
			continue
		}
		if binding.ConfigurationPending {
			return state, fmt.Errorf("binding %s is pending", binding.BindingUID)
		}
		enabled[binding.BindingUID] = true
		public := binding
		public.Zones = nil
		render.ReadyMembers[binding.BindingUID] = agent.eligibleRegionalMembers(public)
		render.ReadyZoneMembers[binding.BindingUID] = map[string]map[string]bool{}
		for _, zone := range binding.Zones {
			desired, ok := state.Publications[zone.ZoneUID]
			applied, approved := active.Publications[zone.ZoneUID]
			if zone.Pending || !ok || !approved || !desired.Verified || !applied.Verified || !equalJSON(desired, applied) {
				return state, fmt.Errorf("zone %s is not currently approved", zone.ZoneUID)
			}
			private := binding
			private.Zones = []model.ZoneAttachment{zone}
			ready := agent.eligibleRegionalMembers(private)
			if len(ready) == 0 {
				return state, fmt.Errorf("zone %s has no verified serving path", zone.ZoneUID)
			}
			render.ReadyZoneMembers[binding.BindingUID][zone.ZoneUID] = ready
		}
	}
	filtered := *state.Snapshot
	filtered.Bindings = nil
	for _, b := range state.Snapshot.Bindings {
		if !b.Tombstone {
			filtered.Bindings = append(filtered.Bindings, b)
		}
	}
	expected, err := agent.renderDesired(render, filtered, enabled)
	if err != nil {
		return state, err
	}
	if hashFiles(expected.Files) != hashFiles(active.Files) {
		return state, errors.New("approved files differ from current runtime settings")
	}
	return state, nil
}
