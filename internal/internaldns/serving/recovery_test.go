// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func revisedPublication(t *testing.T, now time.Time, revision uint64, generation uint64, address string) (model.PublicationChunk, model.PublicationManifest) {
	t.Helper()
	plan := model.PublicationPlan{ZoneUID: "zone-a", Apex: "prod.internal", ObservationFences: []model.ObservationFence{{ContributionUID: "contrib-a", Sequence: generation, Generation: generation}}, Owners: []string{"api.prod.internal"}, RRSets: []model.RRSet{{Name: "api.prod.internal", Type: "A", TTL: 30, Records: []model.RRRecord{{Content: address, ContributionUID: "contrib-a", Sequence: generation, Eligible: true, Generation: generation}}}}}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	name := "manifest-r" + strings.Repeat("i", int(revision))
	chunk := model.PublicationChunk{ManifestUID: name, ZoneUID: "zone-a", WriterEpoch: 2, Revision: revision, Index: 0, SHA256: model.Hash(payload), Payload: payload}
	manifest := model.PublicationManifest{ManifestUID: name, ZoneUID: "zone-a", Apex: "prod.internal", WriterEpoch: 2, Revision: revision, PreviousRevision: revision - 1, Chunks: []model.ChunkRef{{Index: 0, SHA256: chunk.SHA256, Size: len(payload)}}, ContentHash: model.Hash(payload), GeneratedAt: now}
	return chunk, manifest
}

func deliverPublication(t *testing.T, a *Agent, chunk model.PublicationChunk, manifest model.PublicationManifest) error {
	t.Helper()
	ctx := context.Background()
	if err := a.Handle(ctx, envelope(t, model.KindPublicationChunk, "zone-a", manifest.WriterEpoch, manifest.Revision, chunk)); err != nil {
		return err
	}
	return a.Handle(ctx, envelope(t, model.KindPublicationManifest, "zone-a", manifest.WriterEpoch, manifest.Revision, manifest))
}

func TestRestartKeepsNewerUpdateAndRejectsDelayedOlderOne(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	a, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, a, now)
	oldChunk, oldManifest := testPublication(t, now)
	newChunk, newManifest := revisedPublication(t, now, 5, 2, "192.0.2.20")
	if err := deliverPublication(t, a, newChunk, newManifest); err != nil {
		t.Fatal(err)
	}
	if data := currentZone(t, a); !strings.Contains(data, "192.0.2.20") || strings.Contains(data, "192.0.2.10") {
		t.Fatalf("the newer update was not applied: %s", data)
	}

	restarted, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if p := restarted.state.Publications["zone-a"]; p.Fence.Revision != 5 {
		t.Fatalf("restart did not reload the newer update from its checkpoint: %#v", p.Fence)
	}
	if f := restarted.state.ContributionFences["contrib-a"]; f.Generation != 2 {
		t.Fatalf("restart did not reload the contribution fence: %#v", f)
	}
	// The store the restarted member bootstraps from still holds the older
	// revision, as a lagging broker would.
	snapshot := testSnapshot(now)
	stale := []model.Envelope{
		envelope(t, model.KindPublicationChunk, "zone-a", 2, 4, oldChunk),
		envelope(t, model.KindServingSnapshot, "shard", 1, 1, snapshot),
		envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, oldManifest),
	}
	if err := restarted.loadBootstrap(context.Background(), bootstrapFunc(func(context.Context) ([]model.Envelope, error) { return stale, nil })); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if data := currentZone(t, restarted); !strings.Contains(data, "192.0.2.20") {
		t.Fatalf("restart lost the newer update: %s", data)
	}

	// A broker redelivers the older revision after the restart.
	if err := deliverPublication(t, restarted, oldChunk, oldManifest); err != nil {
		t.Fatal(err)
	}
	// A newer revision compiled from the older observation must not apply either.
	staleChunk, staleManifest := revisedPublication(t, now, 6, 1, "192.0.2.10")
	if err := deliverPublication(t, restarted, staleChunk, staleManifest); err == nil || !strings.Contains(err.Error(), "regressed") {
		t.Fatalf("a revision built from an older observation must be rejected as a regression, got %v", err)
	}
	if err := restarted.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := restarted.state.Publications["zone-a"]
	if p.Fence.Revision != 5 || !p.Verified {
		t.Fatalf("delayed data moved the applied publication: %#v", p.Fence)
	}
	if data := currentZone(t, restarted); !strings.Contains(data, "192.0.2.20") || strings.Contains(data, "192.0.2.10") {
		t.Fatalf("delayed data restored an outdated answer: %s", data)
	}
}
