package serving

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestBindingWithdrawalPreservesSourceSequenceAndAllowsNormalRenewal(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	clock := now
	cfg := regionalTestConfig(t)
	a, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	active := testSnapshot(now)
	active.Bindings[0].Version.Sequence = 2
	if err := a.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 1, active)); err != nil {
		t.Fatal(err)
	}
	chunk, manifest := testPublication(t, now)
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationChunk, "zone-a", 2, 4, chunk)); err != nil {
		t.Fatal(err)
	}
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, manifest)); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(11 * time.Second)
	withdrawn := active
	withdrawn.Bindings = append([]model.Binding(nil), active.Bindings...)
	withdrawn.ConfigurationRevision = 2
	withdrawn.Bindings[0].ConfigurationRevision = 2
	withdrawn.Bindings[0].Tombstone = true
	if err := a.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 2, withdrawn)); err != nil {
		t.Fatal(err)
	}
	if f := a.state.BindingFences["binding-a"]; !f.Tombstone || f.Version.Sequence != 2 {
		t.Fatal("withdrawal fabricated access version")
	}
	// Withdrawal preserves the source sequence so a newer access version can reactivate.
	renewed := withdrawn
	renewed.Bindings = append([]model.Binding(nil), withdrawn.Bindings...)
	renewed.ConfigurationRevision = 3
	renewed.Bindings[0].ConfigurationRevision = 3
	renewed.Bindings[0].Tombstone = false
	renewed.Bindings[0].Version.Sequence = 3
	if err := a.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 3, renewed)); err != nil {
		t.Fatalf("normal source2 -> source3 renewal rejected: %v", err)
	}
	config, err := os.ReadFile(cfg.Render.ClusterCoreDNS.Path)
	if err != nil || !strings.Contains(string(config), renewed.Bindings[0].ViewName()) {
		t.Fatalf("renewal did not restore context: %v", err)
	}
	if err := a.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 2, withdrawn)); err != nil {
		t.Fatal(err)
	}
	if a.state.Snapshot.Bindings[0].Tombstone || a.state.ServingFence.Revision != 3 {
		t.Fatal("stale withdrawal replay removed current access")
	}
	// A higher shard revision cannot legitimize an older source revision.
	stale := withdrawn
	stale.ConfigurationRevision = 4
	if err := a.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 4, stale)); err == nil {
		t.Fatal("new shard revision accepted stale binding authority")
	}
	if a.state.ServingFence.Revision != 3 {
		t.Fatal("rejected binding authority advanced shard fence")
	}
}

func TestBindingOmissionFenceSurvivesRestartAndRequiresNewSourceAuthority(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	a, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	active := testSnapshot(now)
	active.Bindings[0].Version.Sequence = 2
	if err := a.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 1, active)); err != nil {
		t.Fatal(err)
	}
	omitted := active
	omitted.ConfigurationRevision = 2
	omitted.Bindings = nil
	if err := a.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 2, omitted)); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.state.BindingFences["binding-a"].Tombstone {
		t.Fatal("restart forgot omitted UID withdrawal")
	}
	candidate := active
	candidate.Bindings = append([]model.Binding(nil), active.Bindings...)
	candidate.ConfigurationRevision = 3
	candidate.Bindings[0].ConfigurationRevision = 2
	if err := restarted.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 3, candidate)); err == nil {
		t.Fatal("same source sequence resurrected omitted binding")
	}
	if err := restarted.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 3, candidate)); err == nil {
		t.Fatal("same source sequence changed its source revision")
	}
	candidate.Bindings[0].Version.Sequence = 3
	if err := restarted.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 3, candidate)); err != nil {
		t.Fatalf("new source sequence could not reactivate omitted binding: %v", err)
	}
	// A replacement resource has a new UID and independent source sequence.
	replacement := candidate
	replacement.Bindings = append([]model.Binding(nil), candidate.Bindings...)
	replacement.ConfigurationRevision = 4
	replacement.Bindings[0].BindingUID = "replacement-binding"
	replacement.Bindings[0].ConfigurationRevision = 1
	replacement.Bindings[0].Version.Sequence = 1
	if err := restarted.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 4, replacement)); err != nil {
		t.Fatal(err)
	}
	if !restarted.state.BindingFences["binding-a"].Tombstone || restarted.state.BindingFences["replacement-binding"].Version.Sequence != 1 {
		t.Fatal("replacement UID discarded old source fences or inherited its sequence")
	}
	candidate.ConfigurationRevision = 5
	candidate.Bindings[0].ConfigurationRevision = 3
	if err := restarted.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 5, candidate)); err == nil {
		t.Fatal("retired UID replay replaced newly installed resource")
	}
	mutated := replacement
	mutated.Bindings = append([]model.Binding(nil), replacement.Bindings...)
	mutated.ConfigurationRevision = 5
	mutated.Bindings[0].ProjectUID = "different-project"
	if err := restarted.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 5, mutated)); err == nil {
		t.Fatal("same binding UID changed pinned project identity")
	}
}

func TestExplicitWithdrawalAfterRetainedStateRestartRemovesEveryMemberAndPreservesPeer(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	active := testSnapshot(now)
	unaffected := active.Bindings[0]
	unaffected.BindingUID, unaffected.ProjectUID, unaffected.ContextUID = "binding-b", "project-b", "context-b"
	unaffected.ConsumerAddress, unaffected.ClusterAddress = "10.253.0.52", "10.253.0.42"
	unaffected.Zones = nil
	active.Bindings = append(active.Bindings, unaffected)
	active.Members = []model.Member{{MemberID: "member-a", ReplicaID: "member-a", Role: "regional"}, {MemberID: "member-b", ReplicaID: "member-b", Role: "regional"}}
	chunk, manifest := testPublication(t, now)

	for _, member := range []string{"member-a", "member-b"} {
		member := member
		t.Run(member, func(t *testing.T) {
			cfg := regionalTestConfig(t)
			cfg.MemberID, cfg.ReplicaID = member, member
			initial, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			if err := initial.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 1, active)); err != nil {
				t.Fatal(err)
			}
			if err := initial.Handle(context.Background(), envelope(t, model.KindPublicationChunk, "zone-a", 2, 4, chunk)); err != nil {
				t.Fatal(err)
			}
			if err := initial.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, manifest)); err != nil {
				t.Fatal(err)
			}

			publisher := &fakePublisher{}
			restarted, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, AckPublisher: publisher, Clock: func() time.Time { return now.Add(time.Second) }})
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.restoreActive(context.Background()); err != nil {
				t.Fatal(err)
			}
			withdrawn := active
			withdrawn.Bindings = append([]model.Binding(nil), active.Bindings...)
			withdrawn.ConfigurationRevision = 2
			withdrawn.Bindings[0].ConfigurationRevision = 2
			withdrawn.Bindings[0].Version.Sequence = 2
			withdrawn.Bindings[0].Tombstone = true
			withdrawn.Bindings[0].Zones = nil
			if err := restarted.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 2, withdrawn)); err != nil {
				t.Fatal(err)
			}
			config, err := os.ReadFile(cfg.Render.ClusterCoreDNS.Path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(config), active.Bindings[0].ViewName()) {
				t.Fatal("withdrawn context remained in active CoreDNS configuration")
			}
			if !strings.Contains(string(config), unaffected.ViewName()) {
				t.Fatal("unaffected context was removed with the deleted network")
			}
			acked := false
			for _, env := range publisher.envs {
				var ack model.MemberAck
				if env.Kind == model.KindMemberAck && json.Unmarshal(env.Payload, &ack) == nil && ack.ResourceUID == active.Bindings[0].BindingUID && ack.Epoch == 1 && ack.Revision == 2 && ack.Phase == model.AckVerified {
					acked = true
				}
			}
			if !acked {
				t.Fatal("member did not acknowledge the exact installed withdrawal")
			}

			afterRestart, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now.Add(2 * time.Second) }})
			if err != nil {
				t.Fatal(err)
			}
			if err := afterRestart.restoreActive(context.Background()); err != nil {
				t.Fatal(err)
			}
			config, err = os.ReadFile(cfg.Render.ClusterCoreDNS.Path)
			if err != nil || strings.Contains(string(config), active.Bindings[0].ViewName()) || !strings.Contains(string(config), unaffected.ViewName()) {
				t.Fatalf("restart restored revoked access or lost unaffected routing: %v", err)
			}
		})
	}
}

func TestCheckpointRejectsPreBindingFenceFormatWithoutDiscardingIt(t *testing.T) {
	t.Parallel()
	for _, version := range []int{2, 3, checkpointFormatVersion} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "checkpoint.json")
			data, err := json.Marshal(map[string]any{"formatVersion": version, "contributionFences": map[string]any{"original-observation": map[string]any{"epoch": 1, "sequence": 2}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := (stateStore{path: path}).Load(); err == nil {
				t.Fatal("checkpoint without complete binding authority history accepted")
			}
			retained, err := os.ReadFile(path)
			if err != nil || string(retained) != string(data) {
				t.Fatal("unsupported checkpoint was modified")
			}
		})
	}
}

func TestPendingConfigurationGatesQueriesAndRecoversWithoutAuthorizationRenewal(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	runner := &recordingRunner{}
	dependencies := Dependencies{Runner: runner, Clock: func() time.Time { return now }}
	agent, err := NewAgent(cfg, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	active := testSnapshot(now)
	healthy := active.Bindings[0]
	healthy.BindingUID, healthy.ProjectUID, healthy.ContextUID = "binding-b", "project-b", "context-b"
	healthy.ConsumerAddress, healthy.ClusterAddress = "10.253.0.52", "10.253.0.42"
	healthy.Zones = nil
	active.Bindings = append(active.Bindings, healthy)
	if err := agent.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 1, active)); err != nil {
		t.Fatal(err)
	}
	chunk, manifest := testPublication(t, now)
	if err := agent.Handle(context.Background(), envelope(t, model.KindPublicationChunk, "zone-a", 2, 4, chunk)); err != nil {
		t.Fatal(err)
	}
	if err := agent.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, manifest)); err != nil {
		t.Fatal(err)
	}
	original := active.Bindings[0].Version
	pending := active
	pending.Bindings = append([]model.Binding(nil), active.Bindings...)
	pending.ConfigurationRevision = 2
	pending.Bindings[0].ConfigurationRevision = 2
	pending.Bindings[0].ConfigurationPending = true
	pending.Bindings[0].Zones = nil
	if err := agent.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 2, pending)); err != nil {
		t.Fatal(err)
	}
	if agent.state.BindingFences["binding-a"].Tombstone || agent.publicationsReady(pending.Bindings[0]) || !agent.publicationsReady(healthy) {
		t.Fatal("pending configuration revoked access, activated incomplete data, or gated another context")
	}
	config, err := os.ReadFile(cfg.Render.ClusterCoreDNS.Path)
	if err != nil {
		t.Fatal(err)
	}
	contexts := decodedCoreDNS(t, config).Contexts
	found := false
	for _, entry := range contexts {
		if entry.ID == pending.Bindings[0].ViewName() {
			found = true
			if !entry.Pending || len(entry.Zones) > 0 {
				t.Fatal("pending context can serve private data")
			}
		}
	}
	if !found {
		t.Fatal("pending context missing")
	}
	restarted, err := NewAgent(cfg, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	ready := active
	ready.Bindings = append([]model.Binding(nil), active.Bindings...)
	ready.ConfigurationRevision = 3
	ready.Bindings[0].ConfigurationRevision = 2
	if err := restarted.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 3, ready)); err != nil {
		t.Fatalf("completion required a new authorization: %v", err)
	}
	if restarted.state.BindingFences["binding-a"].Version != original || !restarted.publicationsReady(ready.Bindings[0]) {
		t.Fatal("completion changed authority or remained gated")
	}
	// The older pending snapshot cannot undo the completed configuration.
	if err := restarted.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 2, pending)); err != nil {
		t.Fatal(err)
	}
	if restarted.state.Snapshot.Bindings[0].ConfigurationPending {
		t.Fatal("stale pending replay gated the recovered context")
	}
}

func TestWithdrawalRenderFailureStopsServing(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	clock := now
	cfg := regionalTestConfig(t)
	runner := &recordingRunner{}
	a, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	active := testSnapshot(now)
	if err := a.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 1, active)); err != nil {
		t.Fatal(err)
	}
	chunk, manifest := testPublication(t, now)
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationChunk, "zone-a", 2, 4, chunk)); err != nil {
		t.Fatal(err)
	}
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, manifest)); err != nil {
		t.Fatal(err)
	}
	if a.state.Active == nil || a.needsActivation {
		t.Fatalf("fixture did not approve serving: active=%t needsActivation=%t", a.state.Active != nil, a.needsActivation)
	}
	gatesBefore := gateCalls(runner)

	a.render = func(RenderConfig, model.ServingSnapshot, map[string]bool, map[string]publicationState) (Rendered, error) {
		return Rendered{}, errors.New("injected render failure")
	}
	clock = now.Add(11 * time.Second)
	withdrawn := active
	withdrawn.Bindings = append([]model.Binding(nil), active.Bindings...)
	withdrawn.ConfigurationRevision = 2
	withdrawn.Bindings[0].ConfigurationRevision = 2
	withdrawn.Bindings[0].Tombstone = true
	if err := a.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 2, withdrawn)); err == nil {
		t.Fatal("withdrawal with a failed render reported success")
	}
	if gateCalls(runner) == gatesBefore || !a.needsActivation {
		t.Fatalf("withdrawal with a failed render kept serving withdrawn data: gates=%d needsActivation=%t", gateCalls(runner)-gatesBefore, a.needsActivation)
	}

	// A render failure that withdraws nothing keeps the approved configuration.
	retained := &recordingRunner{}
	b, err := NewAgent(regionalTestConfig(t), Dependencies{Runner: retained, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 1, active)); err != nil {
		t.Fatal(err)
	}
	if err := b.Handle(context.Background(), envelope(t, model.KindPublicationChunk, "zone-a", 2, 4, chunk)); err != nil {
		t.Fatal(err)
	}
	if err := b.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, manifest)); err != nil {
		t.Fatal(err)
	}
	retainedBefore := gateCalls(retained)
	b.render = a.render
	if err := b.reconcile(context.Background()); err == nil {
		t.Fatal("injected render failure was not returned")
	}
	if gateCalls(retained) != retainedBefore || b.needsActivation {
		t.Fatal("a render failure that withdraws nothing gated serving")
	}
}

func gateCalls(r *recordingRunner) int {
	n := 0
	for _, c := range r.calls {
		if c.Path == "gate" {
			n++
		}
	}
	return n
}
