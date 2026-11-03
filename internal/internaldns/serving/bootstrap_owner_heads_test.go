package serving

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestBootstrapSelectsNewestServingOwnerHeadEveryRole(t *testing.T) {
	for _, role := range []string{"node authority", "node resolver authority off", "regional authority", "regional router authority off"} {
		t.Run(role, func(t *testing.T) {
			now := time.Now().UTC()
			old := testSnapshot(now)
			old.ConfigurationEpoch = 10
			old.ConfigurationRevision = 900
			latest := testSnapshot(now)
			latest.ConfigurationEpoch = 11
			latest.ConfigurationRevision = 1
			latest.Bindings = nil
			latest.Members = []model.Member{
				{MemberID: "node-0", ReplicaID: "node-0", Role: "node"},
				{MemberID: "regional-0", ReplicaID: "regional-0", Role: "regional"},
				{MemberID: "router-0", ReplicaID: "router-0", Role: "resolver"},
			}
			oldEnv := envelope(t, model.KindServingSnapshot, "retained-owner-uid", 10, 900, old)
			newEnv := envelope(t, model.KindServingSnapshot, "current-owner-uid", 11, 1, latest)
			oldKey, err := model.SnapshotKey(oldEnv)
			if err != nil {
				t.Fatal(err)
			}
			newKey, err := model.SnapshotKey(newEnv)
			if err != nil {
				t.Fatal(err)
			}
			if oldKey == newKey {
				t.Fatal("fixture must model distinct legitimate retained serving KV keys")
			}

			cfg := nodeAuthoritativeTestConfig(t)
			switch role {
			case "node resolver authority off":
				cfg.Mode = ModeResolver
				cfg.NodeAuthoritative = false
			case "regional authority":
				cfg = regionalTestConfig(t)
			case "regional router authority off":
				cfg = regionalTestConfig(t)
				cfg.Mode = ModeResolver
				cfg.Render.Role = RenderRegionalDNSDist
				cfg.Render.ClusterDNSDist.Path = filepath.Join(t.TempDir(), "regional-router.conf")
			}
			a, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}})
			if err != nil {
				t.Fatal(err)
			}
			loader := SnapshotBootstrap{Store: &bootstrapLister{results: [][]model.Envelope{{oldEnv, newEnv}}}, Region: "east", Shard: "s1", Attempts: 1}
			if err := a.loadBootstrap(context.Background(), loader); err != nil {
				t.Fatalf("legitimate retained/current serving heads rejected: %v", err)
			}
			if !a.bootstrapComplete || a.state.ServingFence.Epoch != 11 || a.state.ServingFence.Revision != 1 || len(a.state.Snapshot.Bindings) != 0 {
				t.Fatal("bootstrap did not select newest complete shard cut; stale tenant access mixed in")
			}
			if err := a.Step(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBootstrapServingOwnerHeadSelectionIsFenceOrdered(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name        string
		leftEpoch   uint64
		leftRev     uint64
		rightEpoch  uint64
		rightRev    uint64
		winnerEpoch uint64
		winnerRev   uint64
	}{
		{name: "epoch outranks revision", leftEpoch: 10, leftRev: 900, rightEpoch: 11, rightRev: 1, winnerEpoch: 11, winnerRev: 1},
		{name: "revision orders one epoch", leftEpoch: 11, leftRev: 1, rightEpoch: 11, rightRev: 2, winnerEpoch: 11, winnerRev: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left := emptySnapshot(now, tc.leftEpoch, tc.leftRev)
			right := emptySnapshot(now, tc.rightEpoch, tc.rightRev)
			leftEnv := envelope(t, model.KindServingSnapshot, "owner-left", tc.leftEpoch, tc.leftRev, left)
			rightEnv := envelope(t, model.KindServingSnapshot, "owner-right", tc.rightEpoch, tc.rightRev, right)
			for _, envs := range [][]model.Envelope{{leftEnv, rightEnv}, {rightEnv, leftEnv}} {
				got, err := verifyBootstrap(envs, "east", "s1")
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != 1 || got[0].Kind != model.KindServingSnapshot || got[0].Epoch != tc.winnerEpoch || got[0].Revision != tc.winnerRev {
					t.Fatalf("selected %#v, want serving fence %d/%d", got, tc.winnerEpoch, tc.winnerRev)
				}
			}
		})
	}
}

func TestBootstrapServingOwnerEqualFenceMustAgree(t *testing.T) {
	now := time.Now().UTC()
	snapshot := emptySnapshot(now, 11, 2)
	left := envelope(t, model.KindServingSnapshot, "owner-left", 11, 2, snapshot)
	right := envelope(t, model.KindServingSnapshot, "owner-right", 11, 2, snapshot)
	got, err := verifyBootstrap([]model.Envelope{left, right}, "east", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ResourceUID != "owner-left" {
		t.Fatalf("identical winning snapshots were not coalesced: %#v", got)
	}

	divergent := snapshot
	divergent.Members = []model.Member{{MemberID: "node-0", ReplicaID: "node-0", Role: "node"}}
	conflict := envelope(t, model.KindServingSnapshot, "owner-conflict", 11, 2, divergent)
	if _, err := verifyBootstrap([]model.Envelope{left, conflict}, "east", "s1"); err == nil || !strings.Contains(err.Error(), "conflicting serving snapshots") {
		t.Fatalf("equal-fence divergent snapshots were not rejected: %v", err)
	}

	older := emptySnapshot(now, 10, 1)
	olderOther := older
	olderOther.Members = []model.Member{{MemberID: "old-node", ReplicaID: "old-node", Role: "node"}}
	newest := emptySnapshot(now, 11, 1)
	olderEnv := envelope(t, model.KindServingSnapshot, "old-owner-a", 10, 1, older)
	olderConflict := envelope(t, model.KindServingSnapshot, "old-owner-b", 10, 1, olderOther)
	newestEnv := envelope(t, model.KindServingSnapshot, "new-owner", 11, 1, newest)
	for _, envs := range [][]model.Envelope{
		{olderEnv, olderConflict, newestEnv},
		{newestEnv, olderConflict, olderEnv},
	} {
		if got, err := verifyBootstrap(envs, "east", "s1"); err != nil || len(got) != 1 || got[0].ResourceUID != "new-owner" {
			t.Fatalf("non-winning ambiguity affected newest selection: events=%#v err=%v", got, err)
		}
	}
}

func TestBootstrapCompletenessUsesOnlyNewestServingCut(t *testing.T) {
	now := time.Now().UTC()
	oldWithRemovedZone := testSnapshot(now)
	oldWithRemovedZone.ConfigurationEpoch = 10
	oldWithRemovedZone.ConfigurationRevision = 900
	latestEmpty := emptySnapshot(now, 11, 1)
	oldEnv := envelope(t, model.KindServingSnapshot, "old-owner", 10, 900, oldWithRemovedZone)
	newEnv := envelope(t, model.KindServingSnapshot, "new-owner", 11, 1, latestEmpty)
	if got, err := verifyBootstrap([]model.Envelope{oldEnv, newEnv}, "east", "s1"); err != nil || len(got) != 1 || got[0].ResourceUID != "new-owner" {
		t.Fatalf("removed-zone dependency from retained owner blocked newest complete cut: events=%#v err=%v", got, err)
	}

	oldComplete := emptySnapshot(now, 10, 900)
	latestIncomplete := testSnapshot(now)
	latestIncomplete.ConfigurationEpoch = 11
	latestIncomplete.ConfigurationRevision = 1
	oldEnv = envelope(t, model.KindServingSnapshot, "old-owner", 10, 900, oldComplete)
	newEnv = envelope(t, model.KindServingSnapshot, "new-owner", 11, 1, latestIncomplete)
	if _, err := verifyBootstrap([]model.Envelope{oldEnv, newEnv}, "east", "s1"); err == nil || !strings.Contains(err.Error(), "no current publication") {
		t.Fatalf("bootstrap fell back from incomplete newest cut to older owner: %v", err)
	}
}

func TestBootstrapServingOwnerCandidatesRemainFailClosed(t *testing.T) {
	now := time.Now().UTC()
	if _, err := verifyBootstrap(nil, "east", "s1"); err == nil || !strings.Contains(err.Error(), "no complete serving snapshot") {
		t.Fatalf("empty bootstrap accepted: %v", err)
	}

	valid := emptySnapshot(now, 1, 1)
	foreign := envelope(t, model.KindServingSnapshot, "owner", 1, 1, valid)
	foreign.Region = "west"
	if _, err := verifyBootstrap([]model.Envelope{foreign}, "east", "s1"); err == nil || !strings.Contains(err.Error(), "foreign region") {
		t.Fatalf("foreign bootstrap accepted: %v", err)
	}

	mismatched := valid
	mismatched.Region = "west"
	mismatchEnv := envelope(t, model.KindServingSnapshot, "owner", 1, 1, mismatched)
	if _, err := verifyBootstrap([]model.Envelope{mismatchEnv}, "east", "s1"); err == nil || !strings.Contains(err.Error(), "identity does not match") {
		t.Fatalf("mismatched payload accepted: %v", err)
	}
}

func emptySnapshot(now time.Time, epoch, revision uint64) model.ServingSnapshot {
	return model.ServingSnapshot{
		Region:                "east",
		Shard:                 "s1",
		ConfigurationEpoch:    epoch,
		ConfigurationRevision: revision,
		GeneratedAt:           now,
		Members:               []model.Member{{MemberID: "regional-0", ReplicaID: "regional-0", Role: "regional"}},
	}
}
