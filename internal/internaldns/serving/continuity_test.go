// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestRejectedExpansionPreservesServingAndRestarts(t *testing.T) {
	now := time.Now().UTC()
	runner := &recordingRunner{}
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, now)
	activeHash := agent.state.RenderedHash
	candidate := *agent.state.Snapshot
	candidate.ConfigurationRevision++
	candidate.Bindings[0].ConfigurationRevision++
	candidate.Bindings[0].Zones = append(candidate.Bindings[0].Zones, model.ZoneAttachment{ZoneUID: "pending-zone", Apex: "pending.internal", Pending: true, RequiredPublicationEpoch: 1, RequiredPublicationRevision: 1})
	runner.failAt = len(runner.calls) + 1
	if err := agent.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", candidate.ConfigurationEpoch, candidate.ConfigurationRevision, candidate)); err == nil {
		t.Fatal("invalid expansion was accepted")
	}
	if agent.state.RenderedHash != activeHash || agent.state.Active == nil || !agent.healthEnabled {
		t.Fatal("invalid expansion interrupted the active configuration")
	}
	now = now.Add(time.Hour)
	if err := agent.refreshActiveHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg.WatchdogLeasePath)
	if err != nil {
		t.Fatal(err)
	}
	var lease watchdogLease
	if err := json.Unmarshal(data, &lease); err != nil || !lease.ValidUntil.After(now) {
		t.Fatalf("active health stopped during rejected update: %v", err)
	}
	restarted, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.restoreActive(context.Background()); err != nil || !restarted.healthEnabled {
		t.Fatalf("restart could not recover active configuration: %v", err)
	}
	withdrawn := candidate
	withdrawn.ConfigurationRevision++
	withdrawn.Bindings = nil
	runner.failAt = len(runner.calls) + 1
	if err := agent.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", withdrawn.ConfigurationEpoch, withdrawn.ConfigurationRevision, withdrawn)); err == nil {
		t.Fatal("failed withdrawal was accepted")
	}
	if agent.state.Active != nil || agent.healthEnabled {
		t.Fatal("failed withdrawal retained revoked access")
	}
}

func TestPendingZoneIsIsolatedFromReadyZone(t *testing.T) {
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, now)
	binding := agent.state.Snapshot.Bindings[0]
	binding.Zones = append(binding.Zones, model.ZoneAttachment{ZoneUID: "pending", Apex: "pending.internal", Pending: true, RequiredPublicationEpoch: 1, RequiredPublicationRevision: 1})
	if zones := agent.readyZones(binding); len(zones) != 1 || zones[0].ZoneUID != "zone-a" {
		t.Fatalf("pending zone suppressed healthy zone: %#v", zones)
	}
	candidate := *agent.state.Snapshot
	candidate.ConfigurationRevision++
	candidate.Bindings[0] = binding
	candidate.Bindings[0].ConfigurationRevision++
	if err := agent.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", candidate.ConfigurationEpoch, candidate.ConfigurationRevision, candidate)); err != nil {
		t.Fatal(err)
	}
	if agent.state.Active == nil || agent.publicationsReady(binding) {
		t.Fatal("partial serving state was not distinguished from full readiness")
	}
	if zone := currentZone(t, agent); zone == "" {
		t.Fatal("ready zone was not materialized")
	}
}
