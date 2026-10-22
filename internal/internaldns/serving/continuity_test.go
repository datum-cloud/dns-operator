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
	if agent.state.RenderedHash != activeHash || agent.state.Active == nil {
		t.Fatal("invalid expansion interrupted the active configuration")
	}
	now = now.Add(time.Hour)
	if !installedFilesMatch(agent.state.Active.Files) {
		t.Fatal("rejected update changed the approved files")
	}
	restarted, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.restoreActive(context.Background()); err != nil {
		t.Fatalf("restart could not recover active configuration: %v", err)
	}
	withdrawn := candidate
	withdrawn.ConfigurationRevision++
	withdrawn.Bindings = nil
	runner.failAt = len(runner.calls) + 1
	if err := agent.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", withdrawn.ConfigurationEpoch, withdrawn.ConfigurationRevision, withdrawn)); err == nil {
		t.Fatal("failed withdrawal was accepted")
	}
	if agent.state.Active != nil {
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
	pub := &fakePublisher{}
	agent.deps.AckPublisher = pub
	now = now.Add(time.Minute)
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
	verified := false
	for _, env := range pub.envs {
		var ack model.MemberAck
		if err := json.Unmarshal(env.Payload, &ack); err != nil {
			t.Fatal(err)
		}
		if ack.Kind == model.KindServingSnapshot && ack.Revision == candidate.ConfigurationRevision && ack.Phase == model.AckVerified {
			verified = true
		}
	}
	if !verified {
		t.Fatal("pending zone prevented resolver access from becoming ready")
	}
}

type blockedValidationRunner struct {
	recordingRunner
	blocked bool
	started chan struct{}
	release chan struct{}
}

func (r *blockedValidationRunner) Run(ctx context.Context, command Command) ([]byte, error) {
	if command.Path == "validate" && r.blocked {
		close(r.started)
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.recordingRunner.Run(ctx, command)
}

func TestApprovedConfigurationRemainsInstalledDuringCandidateValidation(t *testing.T) {
	now := time.Now().UTC()
	runner := &blockedValidationRunner{started: make(chan struct{}), release: make(chan struct{})}
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, now)
	runner.blocked = true
	candidate := *agent.state.Snapshot
	candidate.ConfigurationRevision++
	candidate.Bindings[0].ConfigurationRevision++
	candidate.Bindings[0].Zones = append(candidate.Bindings[0].Zones, model.ZoneAttachment{ZoneUID: "pending", Apex: "pending.internal", Pending: true, RequiredPublicationEpoch: 1, RequiredPublicationRevision: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- agent.Handle(ctx, envelope(t, model.KindServingSnapshot, "shard", candidate.ConfigurationEpoch, candidate.ConfigurationRevision, candidate))
	}()
	select {
	case <-runner.started:
	case <-ctx.Done():
		t.Fatal("candidate validation did not start")
	}
	if !installedFilesMatch(agent.state.Active.Files) || activationPending(cfg.StateDir) {
		t.Fatal("candidate validation interrupted the approved configuration")
	}
	close(runner.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFailedRestartInstallationRequiresActivation(t *testing.T) {
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, time.Now().UTC())
	for path := range agent.state.Active.Files {
		if err := os.WriteFile(path, []byte("incomplete configuration"), 0644); err != nil {
			t.Fatal(err)
		}
		break
	}
	runner := &recordingRunner{failAt: 1}
	restarted, err := NewAgent(cfg, Dependencies{Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.restoreActive(context.Background()); err == nil {
		t.Fatal("failed installation was accepted")
	}
	if !restarted.needsActivation {
		t.Fatal("failed installation must force daemon activation on retry")
	}
	runner.failAt = 0
	if err := restarted.restoreActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restarted.needsActivation || !installedFilesMatch(restarted.state.Active.Files) {
		t.Fatal("successful retry did not restore approved configuration")
	}
}
