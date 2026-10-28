// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestAgentPublicationLagMetricsAggregateZones(t *testing.T) {
	a := &Agent{
		cfg: Config{Region: "metrics-region", Shard: "metrics-shard", Mode: ModeCombined, NodeAuthoritative: true},
		state: checkpoint{
			Snapshot: &model.ServingSnapshot{Bindings: []model.Binding{
				{Zones: []model.ZoneAttachment{
					{ZoneUID: "behind", RequiredPublicationEpoch: 2, RequiredPublicationRevision: 4},
					{ZoneUID: "missing", RequiredPublicationEpoch: 3, RequiredPublicationRevision: 1},
				}},
				{Zones: []model.ZoneAttachment{{ZoneUID: "behind", RequiredPublicationEpoch: 2, RequiredPublicationRevision: 4}}},
			}},
			Publications: map[string]publicationState{
				"behind": {Fence: fence{Epoch: 2, Revision: 8}, Verified: true},
			},
			Active: &activeConfiguration{Publications: map[string]publicationState{
				"behind": {Fence: fence{Epoch: 2, Revision: 4}, Verified: true},
			}},
		},
	}
	a.observePublicationLag()
	labels := []string{"metrics-region", "metrics-shard", "node-authoritative"}
	if got := testutil.ToFloat64(agentPublicationLaggingZones.WithLabelValues(labels...)); got != 2 {
		t.Fatalf("lagging zones = %v, want 2", got)
	}
	if got := testutil.ToFloat64(agentPublicationRevisionLag.WithLabelValues(labels...)); got != 4 {
		t.Fatalf("revision lag = %v, want 4", got)
	}
}

func TestAgentPublicationLagUsesActivatedNotReceivedRevision(t *testing.T) {
	a := &Agent{
		cfg: Config{Region: "active-region", Shard: "active-shard", Mode: ModeCombined, NodeAuthoritative: true},
		state: checkpoint{
			Snapshot:     &model.ServingSnapshot{Bindings: []model.Binding{{Zones: []model.ZoneAttachment{{ZoneUID: "zone", RequiredPublicationEpoch: 4, RequiredPublicationRevision: 6}}}}},
			Publications: map[string]publicationState{"zone": {Fence: fence{Epoch: 4, Revision: 9}, Verified: true}},
			Active:       &activeConfiguration{Publications: map[string]publicationState{"zone": {Fence: fence{Epoch: 4, Revision: 6}, Verified: true}}},
		},
	}
	a.observePublicationLag()
	labels := []string{"active-region", "active-shard", "node-authoritative"}
	if got := testutil.ToFloat64(agentPublicationLaggingZones.WithLabelValues(labels...)); got != 1 {
		t.Fatalf("lagging zones = %v, want 1", got)
	}
	if got := testutil.ToFloat64(agentPublicationRevisionLag.WithLabelValues(labels...)); got != 3 {
		t.Fatalf("revision lag = %v, want 3", got)
	}
}

func TestAgentPublicationLagHandlesEpochAndWithdrawalWatermarks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		desired  publicationState
		active   publicationState
		wantLag  float64
		wantRevs float64
	}{
		{
			name:     "new writer epoch",
			desired:  publicationState{Fence: fence{Epoch: 3, Revision: 1}},
			active:   publicationState{Fence: fence{Epoch: 2, Revision: 99}, Verified: true},
			wantLag:  1,
			wantRevs: 0,
		},
		{
			name:     "pending withdrawal",
			desired:  publicationState{Fence: fence{Epoch: 2, Revision: 5}, Manifest: model.PublicationManifest{Tombstone: true}},
			active:   publicationState{Fence: fence{Epoch: 2, Revision: 4}, Verified: true},
			wantLag:  1,
			wantRevs: 1,
		},
		{
			name:     "applied withdrawal",
			desired:  publicationState{Fence: fence{Epoch: 2, Revision: 5}, Manifest: model.PublicationManifest{Tombstone: true}},
			active:   publicationState{Fence: fence{Epoch: 2, Revision: 5}, Manifest: model.PublicationManifest{Tombstone: true}, Verified: true},
			wantLag:  0,
			wantRevs: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			region := "watermark-" + strings.ReplaceAll(tc.name, " ", "-")
			a := &Agent{
				cfg: Config{Region: region, Shard: "metrics-shard", Mode: ModeCombined, NodeAuthoritative: true},
				state: checkpoint{
					Snapshot:     &model.ServingSnapshot{Bindings: []model.Binding{{Zones: []model.ZoneAttachment{{ZoneUID: "zone", RequiredPublicationEpoch: 2, RequiredPublicationRevision: 4}}}}},
					Publications: map[string]publicationState{"zone": tc.desired},
					Active:       &activeConfiguration{Publications: map[string]publicationState{"zone": tc.active}},
				},
			}
			a.observePublicationLag()
			labels := []string{region, "metrics-shard", "node-authoritative"}
			if got := testutil.ToFloat64(agentPublicationLaggingZones.WithLabelValues(labels...)); got != tc.wantLag {
				t.Fatalf("lagging zones = %v, want %v", got, tc.wantLag)
			}
			if got := testutil.ToFloat64(agentPublicationRevisionLag.WithLabelValues(labels...)); got != tc.wantRevs {
				t.Fatalf("revision lag = %v, want %v", got, tc.wantRevs)
			}
		})
	}
}

func TestAgentPublicationLagTracksFailedOrdinaryUpdateUntilActivation(t *testing.T) {
	now := time.Now().UTC()
	cfg := nodeAuthoritativeTestConfig(t)
	runner := &recordingRunner{}
	a, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, a, now)
	labels := []string{cfg.Region, cfg.Shard, "node-authoritative"}
	if got := testutil.ToFloat64(agentPublicationLaggingZones.WithLabelValues(labels...)); got != 0 {
		t.Fatalf("initial lagging zones = %v, want 0", got)
	}

	chunk, manifest := testPublication(t, now)
	var plan model.PublicationPlan
	if err := json.Unmarshal(chunk.Payload, &plan); err != nil {
		t.Fatal(err)
	}
	plan.RRSets[0].Records = append(plan.RRSets[0].Records, model.RRRecord{Content: "192.0.2.11", ContributionUID: "contrib-a", Sequence: 1, Eligible: true, Generation: 1})
	chunk.Payload, err = json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	chunk.ManifestUID = "manifest-b"
	chunk.Revision = 5
	chunk.SHA256 = model.Hash(chunk.Payload)
	manifest.ManifestUID = chunk.ManifestUID
	manifest.PreviousRevision = 4
	manifest.Revision = chunk.Revision
	manifest.ContentHash = chunk.SHA256
	manifest.Chunks = []model.ChunkRef{{Index: 0, SHA256: chunk.SHA256, Size: len(chunk.Payload)}}
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationChunk, "zone-a", 2, 5, chunk)); err != nil {
		t.Fatal(err)
	}
	runner.failAt = len(runner.calls) + 1
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 5, manifest)); err == nil {
		t.Fatal("failed ordinary publication activation was accepted")
	}
	if floor := a.state.Snapshot.Bindings[0].Zones[0].RequiredPublicationRevision; floor != 4 {
		t.Fatalf("ordinary update changed attachment floor to %d", floor)
	}
	if a.state.Publications["zone-a"].Fence.Revision != 5 || a.state.Active.Publications["zone-a"].Fence.Revision != 4 {
		t.Fatalf("test did not retain active revision 4 behind desired revision 5: desired=%#v active=%#v", a.state.Publications["zone-a"].Fence, a.state.Active.Publications["zone-a"].Fence)
	}
	if got := testutil.ToFloat64(agentPublicationLaggingZones.WithLabelValues(labels...)); got != 1 {
		t.Fatalf("failed update lagging zones = %v, want 1", got)
	}
	if got := testutil.ToFloat64(agentPublicationRevisionLag.WithLabelValues(labels...)); got != 1 {
		t.Fatalf("failed update revision lag = %v, want 1", got)
	}

	runner.failAt = 0
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 5, manifest)); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(agentPublicationLaggingZones.WithLabelValues(labels...)); got != 0 {
		t.Fatalf("activated update lagging zones = %v, want 0", got)
	}
	if got := testutil.ToFloat64(agentPublicationRevisionLag.WithLabelValues(labels...)); got != 0 {
		t.Fatalf("activated update revision lag = %v, want 0", got)
	}
}
