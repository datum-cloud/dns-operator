// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type activeConfiguration struct {
	Snapshot     model.ServingSnapshot       `json:"snapshot"`
	Publications map[string]publicationState `json:"publications"`
	Files        map[string][]byte           `json:"files"`
}

func (a *Agent) gateRejectedUpdate(ctx context.Context, err error) {
	var preserved preservedTransactionError
	if errors.As(err, &preserved) && a.canRetainActive() {
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
		if !ok || candidate.Tombstone || candidate.ConfigurationPending || bindingIdentity(binding) != bindingIdentity(candidate) || binding.Authorization != candidate.Authorization {
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
		return nil
	}
	a.state.Active = &active
	if err := a.store.Save(a.state); err != nil {
		return err
	}
	a.healthMu.Lock()
	a.healthEnabled = true
	a.healthMu.Unlock()
	return nil
}

func (a *Agent) suspendActiveHealth() {
	a.healthMu.Lock()
	defer a.healthMu.Unlock()
	a.healthEnabled = false
	if a.cfg.WatchdogLeasePath != "" {
		_ = os.Remove(a.cfg.WatchdogLeasePath)
	}
}

func (a *Agent) restoreActive(ctx context.Context) error {
	if !a.canRetainActive() {
		return nil
	}
	if err := a.tx.ApplyForce(ctx, a.state.Active.Files); err != nil {
		return err
	}
	a.healthMu.Lock()
	a.healthEnabled = true
	a.healthMu.Unlock()
	return nil
}

func (a *Agent) runActiveHealth(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.refreshActiveHealth(ctx); err != nil {
				a.report(err)
			}
		}
	}
}

func (a *Agent) refreshActiveHealth(ctx context.Context) error {
	a.healthMu.Lock()
	enabled := a.healthEnabled
	a.healthMu.Unlock()
	if !enabled || a.cfg.WatchdogLeasePath == "" {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := a.probeLocalDaemons(probeCtx); err != nil {
		return err
	}
	a.healthMu.Lock()
	defer a.healthMu.Unlock()
	if !a.healthEnabled {
		return nil
	}
	return a.writeWatchdogLease()
}

func (a *Agent) probeLocalDaemons(ctx context.Context) error {
	if a.cfg.UnsafeAllowNoDNSVerify {
		return nil
	}
	role := a.cfg.Render.Role
	if role == "" {
		role = RenderBoth
	}
	var binds []BINDRenderConfig
	var dists []DNSDistRenderConfig
	if role == RenderNode || role == RenderBoth {
		binds = append(binds, a.cfg.Render.NodeBIND)
		dists = append(dists, a.cfg.Render.NodeDNSDist)
	}
	if role == RenderCluster || role == RenderBoth || role == RenderRegionalBIND {
		binds = append(binds, a.cfg.Render.ClusterBIND)
	}
	if role == RenderCluster || role == RenderBoth || role == RenderRegionalDNSDist {
		dists = append(dists, a.cfg.Render.ClusterDNSDist)
	}
	for _, bind := range binds {
		server := net.JoinHostPort(bind.ListenAddress, strconv.Itoa(int(bind.Port)))
		view := PublicationView{DestinationAddress: bind.ListenAddress, DestinationPort: bind.Port}
		for _, network := range []string{networkUDP, networkTCP} {
			answer, err := exchangePublication(ctx, network, server, ".", view, 200*time.Millisecond)
			if err != nil || answer == nil || !answer.Response {
				return fmt.Errorf("local BIND health %s: %w", server, errors.Join(err, errors.New("DNS response required")))
			}
		}
	}
	for _, dist := range dists {
		for _, network := range []string{networkUDP, networkTCP} {
			answer, err := exchangeResolverSOA(ctx, network, dist.ListenAddress, ".", 200*time.Millisecond)
			if err != nil || answer == nil || !answer.Response {
				return fmt.Errorf("local dnsdist health %s: %w", dist.ListenAddress, errors.Join(err, errors.New("DNS response required")))
			}
		}
	}
	return nil
}
