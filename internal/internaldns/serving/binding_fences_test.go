package serving

import (
	"context"
	"encoding/json"
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
