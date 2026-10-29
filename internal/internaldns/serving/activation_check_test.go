// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestCurrentActivationRejectsIncompleteRollout(t *testing.T) {
	for _, change := range []string{"approved", "pending", "snapshot", "publication", "files", "settings", "mode", "missing"} {
		t.Run(change, func(t *testing.T) {
			now := time.Now().UTC()
			cfg := nodeAuthoritativeTestConfig(t)
			a, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			installTestPublication(t, a, now)
			switch change {
			case "pending":
				if err := a.tx.beginActivation(); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = a.tx.abandonActivation() }()
			case "snapshot":
				a.state.Snapshot.ConfigurationRevision++
			case "publication":
				p := a.state.Publications["zone-a"]
				p.Fence.Revision++
				p.Verified = false
				a.state.Publications["zone-a"] = p
			case "files":
				if err := os.WriteFile(cfg.Render.NodeDNSDist.Path, []byte("wrong"), 0600); err != nil {
					t.Fatal(err)
				}
			case "settings":
				cfg.Render.NodeDNSDist.ListenerCount = 2
			case "mode":
				cfg.NodeAuthoritative = false
			case "missing":
				a.state.Active = nil
			}
			if err := a.store.Save(a.state); err != nil {
				t.Fatal(err)
			}
			called := false
			_, err = checkActivation(context.Background(), cfg, func(context.Context, Config) error { called = true; return nil })
			if change == "approved" {
				if err != nil || !called {
					t.Fatalf("approved activation rejected: %v", err)
				}
			} else if err == nil || called {
				t.Fatalf("incomplete activation reached probes: %v", err)
			}
		})
	}
}

func TestActivationCheckRechecksAfterProbes(t *testing.T) {
	now := time.Now().UTC()
	cfg := nodeAuthoritativeTestConfig(t)
	a, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, a, now)
	failed := errors.New("consumer probe failed")
	if _, err := checkActivation(context.Background(), cfg, func(context.Context, Config) error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("probe failure lost: %v", err)
	}
	_, err = checkActivation(context.Background(), cfg, func(context.Context, Config) error {
		a.state.Snapshot.ConfigurationRevision++
		return a.store.Save(a.state)
	})
	if err == nil {
		t.Fatal("state changed during probes but cleanup was allowed")
	}
}
