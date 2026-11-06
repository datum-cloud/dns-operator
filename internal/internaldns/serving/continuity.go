// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type activeConfiguration struct {
	Snapshot          model.ServingSnapshot       `json:"snapshot"`
	Publications      map[string]publicationState `json:"publications"`
	Files             map[string][]byte           `json:"files"`
	NodeAuthoritative bool                        `json:"nodeAuthoritative,omitempty"`
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
	if old.NodeAuthoritative != a.cfg.NodeAuthoritative {
		return false
	}
	next := map[string]model.Binding{}
	for _, binding := range a.state.Snapshot.Bindings {
		next[binding.BindingUID] = binding
	}
	for _, binding := range old.Snapshot.Bindings {
		// The approved configuration answers nothing for a withdrawn or
		// pending context, so whatever the snapshot now says about it
		// withdraws nothing. Shards carry such contexts until every member
		// confirms them, which would otherwise drop approval on every update.
		// A pending context carries no zones (Snapshot.Validate), so it has
		// no zone data to withdraw either.
		if binding.Tombstone || binding.ConfigurationPending {
			continue
		}
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

// Only an unchanged authorized view may keep its proven routing while an
// additive publication is installed and verified. Withdrawal uses normal gating.
func (a *Agent) retainedNodeRouting() []byte {
	if !a.cfg.NodeAuthoritative || a.needsActivation || !a.canRetainActive() {
		return nil
	}
	active := a.state.Active
	if !equalJSON(active.Snapshot, *a.state.Snapshot) || a.state.RenderedHash != hashFiles(active.Files) || activationPending(a.cfg.StateDir) || !installedFilesMatch(active.Files) {
		return nil
	}
	renderConfig := a.cfg.Render
	renderConfig.NodeAuthoritative = true
	renderConfig.ReadyZoneMembers = map[string]map[string]map[string]bool{}
	ready := map[string]bool{}
	// Render withdrawn contexts the way reconciliation does: not at all.
	served := active.Snapshot
	served.Bindings = nil
	for _, binding := range active.Snapshot.Bindings {
		if binding.Tombstone {
			continue
		}
		served.Bindings = append(served.Bindings, binding)
		ready[binding.BindingUID] = !binding.ConfigurationPending
		renderConfig.ReadyZoneMembers[binding.BindingUID] = map[string]map[string]bool{}
		for _, zone := range binding.Zones {
			publication, ok := active.Publications[zone.ZoneUID]
			if !ok || zone.Pending || !publication.Verified || publication.Manifest.Tombstone || model.Compare(publication.Fence.Epoch, publication.Fence.Revision, zone.RequiredPublicationEpoch, zone.RequiredPublicationRevision) < 0 {
				return nil
			}
			renderConfig.ReadyZoneMembers[binding.BindingUID][zone.ZoneUID] = map[string]bool{a.cfg.MemberID: true}
		}
	}
	rendered, err := renderPublications(renderConfig, served, ready, active.Publications)
	if err != nil || string(rendered.Files[renderConfig.NodeDNSDist.Path]) != string(active.Files[renderConfig.NodeDNSDist.Path]) {
		return nil
	}
	return active.Files[renderConfig.NodeDNSDist.Path]
}

// withdrawApproval stops retaining the approved configuration because the
// desired state withdraws something it serves. The daemons keep answering
// from it until the withdrawal is installed, so readiness keeps it while this
// agent holds the activation lock. Restart and retention never use it.
func (a *Agent) withdrawApproval() {
	if a.state.Active != nil {
		a.state.Withdrawing = a.state.Active
	}
	a.state.Active = nil
}

func (a *Agent) approveConfiguration(files map[string][]byte) error {
	candidate := activeConfiguration{Snapshot: *a.state.Snapshot, Publications: a.state.Publications, Files: files, NodeAuthoritative: a.cfg.NodeAuthoritative}
	data, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	var active activeConfiguration
	if err := json.Unmarshal(data, &active); err != nil {
		return err
	}
	if a.state.Active != nil && a.state.Withdrawing == nil && equalJSON(*a.state.Active, active) {
		return a.tx.commitActivation()
	}
	previous, withdrawing := a.state.Active, a.state.Withdrawing
	a.state.Active, a.state.Withdrawing = &active, nil
	if err := a.store.Save(a.state); err != nil {
		a.state.Active, a.state.Withdrawing = previous, withdrawing
		return err
	}
	return a.tx.commitActivation()
}

const localDaemonStartGrace = 10 * time.Second

const restoredCheckpointLimit = 15 * time.Minute

func (a *Agent) restoredCheckpointExpired() bool {
	return !a.restoredAt.IsZero() && !a.bootstrapComplete && !a.deps.Clock().Before(a.restoredAt.Add(restoredCheckpointLimit))
}

func (a *Agent) withdrawRestoredCheckpoint(ctx context.Context) error {
	if a.checkpointWithdrawn {
		return nil
	}
	a.checkpointWithdrawn = true
	return errors.Join(fmt.Errorf("no DNS state received within %s of restart, withdrawing the restored configuration", restoredCheckpointLimit), a.failClosed(ctx))
}

func (a *Agent) awaitLocalDaemons(ctx context.Context) error {
	deadline := a.deps.Clock().Add(localDaemonStartGrace)
	for {
		err := a.probeLocalDaemons(ctx)
		if err == nil || ctx.Err() != nil {
			return err
		}
		if !a.deps.Clock().Before(deadline) {
			a.report(fmt.Errorf("local daemons did not answer within %s, reapplying the approved configuration: %w", localDaemonStartGrace, err))
			return err
		}
		if !waitRetry(ctx, 200*time.Millisecond) {
			return ctx.Err()
		}
	}
}

const localServingWait = 30 * time.Second

// awaitLocalServing holds query verification until the member's own daemons
// answer the readiness canary, once per agent-driven daemon restart and again
// after a failed context probe while they last answered. After a wait runs
// out, later checks probe once without waiting until the daemons answer again.
// A probe answered before CoreDNS loads would otherwise gate every context it
// checks. It holds the agent lock, so the wait is bounded.
func (a *Agent) awaitLocalServing(ctx context.Context) {
	if a.deps.LocalServing == nil {
		return
	}
	if a.localServingRestarts != a.tx.restarts {
		a.localServingAwaited = false
		a.localServingRestarts = a.tx.restarts
	}
	if a.localServingAwaited {
		return
	}
	a.localServingAwaited = true
	wait := a.localServingWait
	if wait <= 0 {
		wait = localServingWait
	}
	if a.localServingExpired {
		wait = 0
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		err := a.deps.LocalServing(ctx)
		a.localServingAnswered = err == nil
		if err == nil {
			a.localServingExpired = false
			return
		}
		if waitCtx.Err() != nil || !waitRetry(waitCtx, 250*time.Millisecond) {
			if ctx.Err() == nil && !a.localServingExpired {
				a.localServingExpired = true
				a.report(fmt.Errorf("local daemons did not answer the readiness canary within %s, verifying anyway: %w", wait, err))
			}
			return
		}
	}
}

// recheckLocalServing makes the next verification check local serving again,
// unless the last check already ran out its wait without an answer.
func (a *Agent) recheckLocalServing() {
	if a.localServingAnswered {
		a.localServingAwaited = false
	}
}

// restoreActive preserves running daemons when their installed configuration
// matches the approved checkpoint. Incomplete installation requires recovery.
func (a *Agent) restoreActive(ctx context.Context) error {
	if !a.canRetainActive() {
		return a.failClosed(ctx)
	}
	if err := a.refreshActiveDNSDist(); err != nil {
		return err
	}
	files := a.state.Active.Files
	if !activationPending(a.cfg.StateDir) && installedFilesMatch(files) {
		if err := a.awaitLocalDaemons(ctx); err == nil {
			a.needsActivation = false
			if actual := hashFiles(files); a.state.RenderedHash != actual {
				a.state.RenderedHash = actual
				return a.store.Save(a.state)
			}
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	a.needsActivation = true
	if err := a.tx.ApplyForce(ctx, files); err != nil {
		return err
	}
	a.state.RenderedHash = hashFiles(files)
	if err := a.store.Save(a.state); err != nil {
		return err
	}
	if err := a.tx.commitActivation(); err != nil {
		return err
	}
	a.needsActivation = false
	return nil
}

func (a *Agent) refreshActiveDNSDist() error {
	role := a.cfg.Render.Role
	var routers []DNSDistRenderConfig
	if role == "" || role == RenderBoth || role == RenderNode {
		routers = append(routers, a.cfg.Render.NodeDNSDist)
	}
	if role == "" || role == RenderBoth || role == RenderCluster || role == RenderRegionalDNSDist {
		routers = append(routers, a.cfg.Render.ClusterDNSDist)
	}
	for _, router := range routers {
		if len(router.Backends) == 0 {
			continue
		}
		retained, ok := a.state.Active.Files[router.Path]
		if !ok {
			continue
		}
		current, err := refreshDNSDistConfiguration(router, retained)
		if err != nil {
			return err
		}
		a.state.Active.Files[router.Path] = current
	}
	return nil
}
