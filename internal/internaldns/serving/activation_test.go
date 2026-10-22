// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestActivationOwnershipGatesAbandonedUpdatesWithoutHeartbeat(t *testing.T) {
	dir := t.TempDir()
	tx := FileTransaction{MarkerPath: activationMarkerPath(dir)}
	if err := tx.beginActivation(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.abandonActivation() })
	start, retain, release := daemonPermission(dir)
	release()
	if start || !retain {
		t.Fatal("installation allowed a new process or stopped approved in-memory DNS")
	}
	if err := tx.filesInstalled(); err != nil {
		t.Fatal(err)
	}
	start, retain, release = daemonPermission(dir)
	release()
	if !start || !retain {
		t.Fatal("live installer could not verify its complete configuration")
	}
	if err := tx.abandonActivation(); err != nil {
		t.Fatal(err)
	}
	start, retain, release = daemonPermission(dir)
	release()
	if start || retain {
		t.Fatal("abandoned activation retained query processes")
	}
	if err := os.Remove(tx.MarkerPath); err != nil {
		t.Fatal(err)
	}
	start, retain, release = daemonPermission(dir)
	release()
	if !start || !retain {
		t.Fatal("completed configuration required agent ownership")
	}
}

func TestPublicationActivationRemainsOwnedUntilDNSProof(t *testing.T) {
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := testSnapshot(now)
	if err := agent.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 1, snapshot)); err != nil {
		t.Fatal(err)
	}
	approved := agent.state.Active
	verified := false
	agent.publicationVerifier = publicationVerifierFunc(func(context.Context, model.PublicationPlan, []PublicationView) error {
		verified = true
		if !activationPending(cfg.StateDir) {
			t.Fatal("activation committed before DNS proof")
		}
		if agent.state.Active != approved {
			t.Fatal("unverified publication replaced approved checkpoint")
		}
		start, retain, release := daemonPermission(cfg.StateDir)
		release()
		if !start || !retain {
			t.Fatal("live verification cannot query installed configuration")
		}
		if err := agent.tx.abandonActivation(); err != nil {
			t.Fatal(err)
		}
		start, retain, release = daemonPermission(cfg.StateDir)
		release()
		if start || retain {
			t.Fatal("abandoned unverified publication remains queryable")
		}
		return errors.New("publication proof interrupted")
	})
	chunk, manifest := testPublication(t, now)
	if err := agent.Handle(context.Background(), envelope(t, model.KindPublicationChunk, "zone-a", 2, 4, chunk)); err != nil {
		t.Fatal(err)
	}
	if err := agent.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, manifest)); err == nil {
		t.Fatal("unverified publication accepted")
	}
	if !verified {
		t.Fatal("publication proof was not attempted")
	}
	if !activationPending(cfg.StateDir) {
		t.Fatal("interrupted proof lost recovery marker")
	}
}

func TestRestoreActiveUsesInstalledConfigurationHash(t *testing.T) {
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, now)
	actual := hashFiles(agent.state.Active.Files)
	agent.state.RenderedHash = "unapproved-candidate"
	if err := agent.store.Save(agent.state); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.restoreActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restarted.state.RenderedHash != actual {
		t.Fatal("restart retained candidate hash for approved files")
	}
	saved, err := restarted.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.RenderedHash != actual {
		t.Fatal("approved installed hash was not checkpointed")
	}
}

func TestServingStateUsesReadOnlyGroupAccess(t *testing.T) {
	dir := t.TempDir()
	checkpoint := dir + "/checkpoint.json"
	if err := os.WriteFile(checkpoint, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareServingState(dir); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{checkpoint, activationMarkerPath(dir) + ".lock"} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0640 {
			t.Fatalf("serving group permissions on %s: %v %v", path, info, err)
		}
	}
	start, retain, release := daemonPermission(dir)
	release()
	if !start || !retain {
		t.Fatal("read-only descriptor cannot check accepted activation")
	}
}
