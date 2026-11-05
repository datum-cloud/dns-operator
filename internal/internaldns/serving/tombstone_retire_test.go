// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestVerifiedTombstoneStopsRenewingAfterRetention(t *testing.T) {
	t.Parallel()
	start := time.Now().UTC()
	now := start
	dir := t.TempDir()
	renderConfig, _ := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeCoreDNS.Path = filepath.Join(dir, "runtime", "node-coredns.json")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterCoreDNS = CoreDNSRenderConfig{}
	publisher := &fakePublisher{}
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "regional-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage"), FailClosed: []Command{{Path: "gate"}}},
		UnsafeAllowNoFailClosed: true, UnsafeAllowNoDNSVerify: true,
		AckInterval: 30 * time.Second,
	}, Dependencies{Clock: func() time.Time { return now }, Runner: &recordingRunner{}, AckPublisher: publisher})
	if err != nil {
		t.Fatal(err)
	}
	gone := publicationState{
		Fence:    fence{Epoch: 3, Revision: 9},
		Manifest: model.PublicationManifest{ManifestUID: "pub-gone", ZoneUID: "zone-gone", Apex: "gone.example.", WriterEpoch: 3, Revision: 9, Tombstone: true},
		Plan:     model.PublicationPlan{ZoneUID: "zone-gone", Apex: "gone.example.", RRSets: []model.RRSet{{Name: "www.gone.example.", Type: "A", Records: []model.RRRecord{{Content: "192.0.2.1", Eligible: true}}}}},
		Verified: true,
	}
	agent.state.Publications["zone-gone"] = gone

	renewals := func() int {
		t.Helper()
		before := len(publisher.envs)
		if err := agent.heartbeatPublications(context.Background()); err != nil {
			t.Fatal(err)
		}
		return len(publisher.envs) - before
	}

	if err := agent.retireTombstones(now); err != nil {
		t.Fatal(err)
	}
	if got := agent.state.Publications["zone-gone"]; !got.WithdrawnAt.Equal(start) || got.Retired {
		t.Fatalf("first sighting did not start the retention clock: %+v", got)
	}
	if n := renewals(); n != 1 {
		t.Fatalf("a withdrawal inside its retention renewed %d acks, want 1", n)
	}

	now = start.Add(tombstoneAckRetention - time.Minute)
	if err := agent.retireTombstones(now); err != nil {
		t.Fatal(err)
	}
	if agent.state.Publications["zone-gone"].Retired {
		t.Fatal("a withdrawal retired before its retention ran out")
	}
	if n := renewals(); n != 1 {
		t.Fatalf("a withdrawal near the end of its retention renewed %d acks, want 1", n)
	}

	now = start.Add(tombstoneAckRetention)
	if err := agent.retireTombstones(now); err != nil {
		t.Fatal(err)
	}
	got := agent.state.Publications["zone-gone"]
	if !got.Retired || len(got.Plan.RRSets) != 0 || got.Plan.Apex != "" {
		t.Fatalf("an expired withdrawal kept its records: %+v", got)
	}
	if got.Fence != gone.Fence || !equalJSON(got.Manifest, gone.Manifest) || !got.Verified {
		t.Fatalf("retirement lost the fence that rejects older replays: %+v", got)
	}
	now = now.Add(time.Hour)
	if n := renewals(); n != 0 {
		t.Fatalf("a retired withdrawal renewed %d acks", n)
	}

	saved, err := agent.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if p := saved.Publications["zone-gone"]; !p.Retired || len(p.Plan.RRSets) != 0 || p.Fence != gone.Fence {
		t.Fatalf("retirement was not persisted: %+v", p)
	}

	older := model.PublicationManifest{ManifestUID: "pub-old", ZoneUID: "zone-gone", Apex: "gone.example.", WriterEpoch: 3, Revision: 8, PreviousRevision: 7, Tombstone: true, Chunks: []model.ChunkRef{}, ContentHash: model.EmptyContentHash(), GeneratedAt: now}
	if err := agent.handleManifest(context.Background(), envelope(t, model.KindPublicationManifest, "zone-gone", 3, 8, older)); err != nil {
		t.Fatal(err)
	}
	if p := agent.state.Publications["zone-gone"]; !p.Manifest.Tombstone || p.Fence != gone.Fence {
		t.Fatalf("an older manifest replaced a retired withdrawal: %+v", p)
	}
}

func TestRetireTombstonesLeavesLivePublicationsAlone(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	dir := t.TempDir()
	renderConfig, _ := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeCoreDNS.Path = filepath.Join(dir, "runtime", "node-coredns.json")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterCoreDNS = CoreDNSRenderConfig{}
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "regional-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage"), FailClosed: []Command{{Path: "gate"}}},
		UnsafeAllowNoFailClosed: true, UnsafeAllowNoDNSVerify: true,
	}, Dependencies{Clock: func() time.Time { return now }, Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	live := publicationState{Fence: fence{Epoch: 1, Revision: 2}, Plan: model.PublicationPlan{ZoneUID: "zone-live", Apex: "live.example."}, Verified: true}
	pending := publicationState{Fence: fence{Epoch: 1, Revision: 3}, Manifest: model.PublicationManifest{ZoneUID: "zone-pending", Tombstone: true}}
	agent.state.Publications["zone-live"] = live
	agent.state.Publications["zone-pending"] = pending
	for _, at := range []time.Time{now, now.Add(2 * tombstoneAckRetention)} {
		if err := agent.retireTombstones(at); err != nil {
			t.Fatal(err)
		}
	}
	if got := agent.state.Publications["zone-live"]; !equalJSON(got, live) {
		t.Fatalf("a live publication changed: %+v", got)
	}
	if got := agent.state.Publications["zone-pending"]; !equalJSON(got, pending) {
		t.Fatalf("an unverified withdrawal changed: %+v", got)
	}
}
