// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestNodePublicationUpdateAvoidsRoutingChurn(t *testing.T) {
	for _, change := range []string{"unchanged", "add", "delete", "replace", "reject"} {
		t.Run(change, func(t *testing.T) {
			now := time.Now().UTC()
			cfg := nodeAuthoritativeTestConfig(t)
			cfg.Transaction.Reload = []Command{
				{Path: "reload-coredns", WhenChanged: []string{cfg.Render.NodeCoreDNS.Path}},
				{Path: "reload-dnsdist", WhenChanged: []string{cfg.Render.NodeDNSDist.Path}},
			}
			runner := &recordingRunner{}
			a, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			installTestPublication(t, a, now)
			baseline, err := os.ReadFile(cfg.Render.NodeDNSDist.Path)
			if err != nil {
				t.Fatal(err)
			}
			runner.calls = nil
			p := a.state.Publications["zone-a"]
			// Copy the plan so the approved checkpoint remains an independent input.
			b, _ := json.Marshal(p.Plan)
			var plan model.PublicationPlan
			if err := json.Unmarshal(b, &plan); err != nil {
				t.Fatal(err)
			}
			if change == "add" {
				plan.RRSets[0].Records = append(plan.RRSets[0].Records, model.RRRecord{Content: "192.0.2.11", Eligible: true})
			}
			if change == "delete" {
				plan.RRSets = nil
			}
			if change == "replace" {
				plan.RRSets[0].Records[0].Content = "192.0.2.99"
			}
			verifiedOnce := false
			a.publicationVerifier = publicationVerifierFunc(func(_ context.Context, _ model.PublicationPlan, _ []PublicationView) error {
				if !verifiedOnce && a.state.Publications["zone-a"].Verified {
					t.Fatal("candidate reported verified before query proof")
				}
				if !verifiedOnce && change != "delete" && change != "replace" && (a.state.Active == nil || a.state.Active.Publications["zone-a"].Fence.Revision != 4) {
					t.Fatal("candidate approved before query proof")
				}
				verifiedOnce = true
				if change == "reject" {
					return errors.New("candidate probe failed")
				}
				return nil
			})
			payload, _ := json.Marshal(plan)
			chunk := model.PublicationChunk{ManifestUID: "manifest-b", ZoneUID: "zone-a", WriterEpoch: 2, Revision: 5, Index: 0, SHA256: model.Hash(payload), Payload: payload}
			manifest := model.PublicationManifest{ManifestUID: chunk.ManifestUID, ZoneUID: "zone-a", Apex: plan.Apex, WriterEpoch: 2, Revision: 5, PreviousRevision: 4, Chunks: []model.ChunkRef{{Index: 0, SHA256: chunk.SHA256, Size: len(payload)}}, ContentHash: model.Hash(payload), GeneratedAt: now}
			ctx := context.Background()
			if err := a.Handle(ctx, envelope(t, model.KindPublicationChunk, "zone-a", 2, 5, chunk)); err != nil {
				t.Fatal(err)
			}
			err = a.Handle(ctx, envelope(t, model.KindPublicationManifest, "zone-a", 2, 5, manifest))
			if change == "reject" {
				if err == nil {
					t.Fatal("failed publication query proof accepted")
				}
				if a.state.Publications["zone-a"].Verified {
					t.Fatal("rejected publication verified")
				}
				found := false
				for _, call := range runner.calls {
					if call.Path == "gate" {
						found = true
					}
				}
				if !found {
					t.Fatal("failed query proof did not fail closed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			reloads := 0
			for _, call := range runner.calls {
				if call.Path == "reload-dnsdist" {
					reloads++
				}
			}
			if change == "delete" || change == "replace" {
				if reloads == 0 {
					t.Fatal("withdrawal retained the old routing during activation")
				}
			} else if reloads != 0 {
				t.Fatalf("ordinary %s update reloaded DNSdist %d times", change, reloads)
			}
			current, err := os.ReadFile(cfg.Render.NodeDNSDist.Path)
			if err != nil {
				t.Fatal(err)
			}
			if string(current) != string(baseline) {
				t.Fatal("final routing changed")
			}
			data, err := os.ReadFile(cfg.Render.NodeCoreDNS.Path)
			if err != nil {
				t.Fatal(err)
			}
			if change == "add" && !strings.Contains(string(data), "192.0.2.11") {
				t.Fatal("new record not installed")
			}
			if (change == "delete" || change == "replace") && strings.Contains(string(data), "192.0.2.10") {
				t.Fatal("deleted record retained")
			}
			if !a.state.Active.Publications["zone-a"].Verified || a.state.Active.Publications["zone-a"].Fence.Revision != 5 {
				t.Fatal("candidate not approved after query proof")
			}
		})
	}
}

func TestRetainedNodeRoutingRequiresUnchangedAuthorization(t *testing.T) {
	for _, change := range []string{"binding-version", "binding-tombstone", "binding-pending", "zone-detached", "zone-pending", "record-withdrawn", "publication-tombstone", "mode", "recovery", "files", "activation", "key", "backend"} {
		t.Run(change, func(t *testing.T) {
			now := time.Now().UTC()
			cfg := nodeAuthoritativeTestConfig(t)
			a, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			installTestPublication(t, a, now)
			if len(a.retainedNodeRouting()) == 0 {
				t.Fatal("approved unchanged routing unavailable")
			}
			switch change {
			case "binding-version":
				a.state.Snapshot.Bindings[0].Version.Sequence++
			case "binding-tombstone":
				a.state.Snapshot.Bindings[0].Tombstone = true
			case "binding-pending":
				a.state.Snapshot.Bindings[0].ConfigurationPending = true
			case "zone-detached":
				a.state.Snapshot.Bindings[0].Zones = nil
			case "zone-pending":
				a.state.Snapshot.Bindings[0].Zones[0].Pending = true
			case "record-withdrawn":
				p := a.state.Publications["zone-a"]
				p.Plan.RRSets[0].Records[0].Eligible = false
				a.state.Publications["zone-a"] = p
			case "publication-tombstone":
				p := a.state.Publications["zone-a"]
				p.Manifest.Tombstone = true
				a.state.Publications["zone-a"] = p
			case "mode":
				a.cfg.NodeAuthoritative = false
			case "recovery":
				a.needsActivation = true
			case "activation":
				if err := os.WriteFile(activationMarkerPath(cfg.StateDir), []byte("pending"), 0600); err != nil {
					t.Fatal(err)
				}
			case "key":
				a.cfg.Render.NodeDNSDist.IdentityKeyFile = "/new/key"
				a.cfg.Render.NodeCoreDNS.IdentityKeyFile = "/new/key"
			case "backend":
				a.cfg.Render.NodeDNSDist.Backends = []model.Backend{{MemberID: "replacement", Address: "192.0.2.22:53"}}
			case "files":
				if err := os.WriteFile(cfg.Render.NodeDNSDist.Path, []byte("unapproved"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if len(a.retainedNodeRouting()) != 0 {
				t.Fatal("unapproved routing retained")
			}
		})
	}
}
