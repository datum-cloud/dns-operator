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
	active.Bindings[0].Authorization.Revision = 2
	active.Bindings[0].Authorization.ValidUntil = now.Add(10 * time.Second)
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
	if f := a.state.BindingFences["binding-a"]; !f.Tombstone || f.Authorization.Revision != 2 || !f.Authorization.ValidUntil.Equal(active.Bindings[0].Authorization.ValidUntil) {
		t.Fatal("withdrawal fabricated source authorization")
	}
	// Source2 -> Source3 is the next legitimate renewal; DNS must not have
	// consumed sequence3 itself while withdrawing the expired authorization.
	renewed := withdrawn
	renewed.Bindings = append([]model.Binding(nil), withdrawn.Bindings...)
	renewed.ConfigurationRevision = 3
	renewed.Bindings[0].ConfigurationRevision = 3
	renewed.Bindings[0].Tombstone = false
	renewed.Bindings[0].Authorization.Revision = 3
	renewed.Bindings[0].Authorization.ValidUntil = now.Add(2 * time.Minute)
	if err := a.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 3, renewed)); err != nil {
		t.Fatalf("normal source2 -> source3 renewal rejected: %v", err)
	}
	config, err := os.ReadFile(cfg.Render.ClusterBIND.Path)
	if err != nil || !strings.Contains(string(config), renewed.Bindings[0].ViewName()) {
		t.Fatalf("renewal did not restore context: %v", err)
	}
	if err := a.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 2, withdrawn)); err != nil {
		t.Fatal(err)
	}
	if a.state.Snapshot.Bindings[0].Tombstone || a.state.ServingFence.Revision != 3 {
		t.Fatal("stale expiry replay withdrew current renewal")
	}
	// A higher shard revision cannot legitimize an older source lease.
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
	active.Bindings[0].Authorization.Revision = 2
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
	candidate.Bindings[0].Authorization.ValidUntil = now.Add(20 * time.Minute)
	if err := restarted.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 3, candidate)); err == nil {
		t.Fatal("same source sequence changed its original deadline")
	}
	candidate.Bindings[0].Authorization.Revision = 3
	if err := restarted.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 3, candidate)); err != nil {
		t.Fatalf("new source sequence could not reactivate omitted binding: %v", err)
	}
	// A replacement resource has a new UID and independent source sequence.
	replacement := candidate
	replacement.Bindings = append([]model.Binding(nil), candidate.Bindings...)
	replacement.ConfigurationRevision = 4
	replacement.Bindings[0].BindingUID = "replacement-binding"
	replacement.Bindings[0].ConfigurationRevision = 1
	replacement.Bindings[0].Authorization.Revision = 1
	if err := restarted.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 4, replacement)); err != nil {
		t.Fatal(err)
	}
	if !restarted.state.BindingFences["binding-a"].Tombstone || restarted.state.BindingFences["replacement-binding"].Authorization.Revision != 1 {
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
	for _, version := range []int{2, checkpointFormatVersion} {
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
