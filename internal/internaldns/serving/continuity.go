// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"errors"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type activeConfiguration struct {
	Snapshot     model.ServingSnapshot       `json:"snapshot"`
	Publications map[string]publicationState `json:"publications"`
	Files        map[string][]byte           `json:"files"`
}

func (a *Agent) gateRejectedUpdate(ctx context.Context, err error) {
	var preserved preservedTransactionError
	var uncertain uncertainTransactionError
	if !errors.As(err, &uncertain) && a.canRetainActive() && (errors.As(err, &preserved) || !a.needsActivation && a.state.RenderedHash == hashFiles(a.state.Active.Files)) {
		return
	}
	_ = a.failClosed(ctx)
}

func (a *Agent) canRetainActive() bool {
	old := a.state.Active
	if old == nil || a.state.Snapshot == nil {
		return false
	}
	next := map[string]model.Binding{}
	for _, binding := range a.state.Snapshot.Bindings {
		next[binding.BindingUID] = binding
	}
	for _, binding := range old.Snapshot.Bindings {
		candidate, ok := next[binding.BindingUID]
		if !ok || candidate.Tombstone || candidate.ConfigurationPending || bindingIdentity(binding) != bindingIdentity(candidate) || binding.Version != candidate.Version {
			return false
		}
		for _, zone := range binding.Zones {
			present := false
			for _, attached := range candidate.Zones {
				if attached.ZoneUID == zone.ZoneUID && attached.Apex == zone.Apex && !attached.Pending {
					present = true
				}
			}
			if !present && !zone.Pending {
				return false
			}
		}
	}
	for uid, publication := range old.Publications {
		if publication.Manifest.Tombstone {
			continue
		}
		candidate, ok := a.state.Publications[uid]
		if !ok || candidate.Manifest.Tombstone || candidate.Plan.Apex != publication.Plan.Apex {
			return false
		}
		records := map[string]bool{}
		for _, rrset := range candidate.Plan.RRSets {
			for _, record := range rrset.Records {
				if record.Eligible {
					records[rrset.Name+"\x00"+rrset.Type+"\x00"+record.Content] = true
				}
			}
		}
		for _, rrset := range publication.Plan.RRSets {
			for _, record := range rrset.Records {
				if record.Eligible && !records[rrset.Name+"\x00"+rrset.Type+"\x00"+record.Content] {
					return false
				}
			}
		}
	}
	return true
}

func (a *Agent) approveConfiguration(files map[string][]byte) error {
	candidate := activeConfiguration{Snapshot: *a.state.Snapshot, Publications: a.state.Publications, Files: files}
	data, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	var active activeConfiguration
	if err := json.Unmarshal(data, &active); err != nil {
		return err
	}
	if a.state.Active != nil && equalJSON(*a.state.Active, active) {
		return a.tx.commitActivation()
	}
	previous := a.state.Active
	a.state.Active = &active
	if err := a.store.Save(a.state); err != nil {
		a.state.Active = previous
		return err
	}
	return a.tx.commitActivation()
}

// restoreActive preserves running daemons when their installed configuration
// matches the approved checkpoint. Incomplete installation requires recovery.
func (a *Agent) restoreActive(ctx context.Context) error {
	if !a.canRetainActive() {
		return a.failClosed(ctx)
	}
	files := a.state.Active.Files
	if !activationPending(a.cfg.StateDir) && installedFilesMatch(files) {
		if err := a.probeLocalDaemons(ctx); err == nil {
			a.needsActivation = false
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err := a.tx.ApplyForce(ctx, files); err != nil {
		return err
	}
	a.state.RenderedHash = hashFiles(files)
	a.needsActivation = false
	if err := a.store.Save(a.state); err != nil {
		return err
	}
	return a.tx.commitActivation()
}
