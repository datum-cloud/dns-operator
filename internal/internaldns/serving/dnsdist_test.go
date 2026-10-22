// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDNSDistKeyPersistsAcrossAgentRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "dnsdist.key")
	if err := prepareDNSDistKey(path); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareDNSDistKey(path); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil || string(first) != string(second) {
		t.Fatal("control identity changed", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(first)))
	if err != nil || len(decoded) != 32 {
		t.Fatal("invalid control identity", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatal("control key must be readable only by its owner and serving group", err)
	}
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareDNSDistKey(path); err == nil {
		t.Fatal("invalid retained key was accepted")
	}
}

func TestDNSDistBootstrapPublishesOnlyCompleteConfiguration(t *testing.T) {
	config, _ := renderFixture(t)
	router := config.NodeDNSDist
	router.Path = filepath.Join(t.TempDir(), "node-dnsdist.conf")
	if err := os.Mkdir(router.Path+".bootstrap", 0700); err != nil {
		t.Fatal(err)
	}
	if err := prepareDNSDistRouter(router); err == nil {
		t.Fatal("failed staging was accepted")
	}
	if _, err := os.Stat(router.Path); !os.IsNotExist(err) {
		t.Fatal("failed staging exposed a startup file", err)
	}
	if err := os.Remove(router.Path + ".bootstrap"); err != nil {
		t.Fatal(err)
	}
	if err := prepareDNSDistRouter(router); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(router.Path)
	if err != nil || !strings.Contains(string(data), "DNSAction.Refused") || !strings.Contains(string(data), "setRules(rules)") {
		t.Fatal("bootstrap must refuse unknown contexts", err)
	}
	accepted := strings.Replace(string(data), "local bindings = {", "local bindings = {\n  [\"fd53::a\"] = {context=\"tenant-a\", ready=true},", 1)
	accepted = strings.ReplaceAll(accepted, "tcpOnly=true,checkTCP=true,", "")
	if err := os.WriteFile(router.Path, []byte(accepted), 0644); err != nil {
		t.Fatal(err)
	}
	if err := prepareDNSDistRouter(router); err != nil {
		t.Fatal(err)
	}
	retained, err := os.ReadFile(router.Path)
	if err != nil {
		t.Fatal(err)
	}
	_, acceptedRules, _ := strings.Cut(accepted, dnsdistRulesBoundary)
	_, retainedRules, _ := strings.Cut(string(retained), dnsdistRulesBoundary)
	if retainedRules != acceptedRules || !strings.Contains(string(retained), "tcpOnly=true,checkTCP=true") {
		t.Fatal("startup must refresh transport and preserve accepted tenant rules")
	}
	if err := os.WriteFile(router.Path, []byte("incomplete configuration"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := prepareDNSDistRouter(router); err == nil {
		t.Fatal("incomplete retained rules must block startup")
	}
}

func TestRestartRetainsRoutingAcrossProcessConfigurationRefresh(t *testing.T) {
	cfg := regionalTestConfig(t)
	cfg.Mode = ModeResolver
	cfg.Render.Role = RenderNode
	cfg.Render.NodeDNSDist.Path = filepath.Join(t.TempDir(), "node-dnsdist.conf")
	_, snapshot := renderFixture(t)
	data := renderDNSDist(cfg.Render.NodeDNSDist, cfg.Render.NodeDNSDist.Backends, snapshot.Bindings, map[string]bool{"bind-a": true}, false, nil, nil)
	retained := []byte(strings.ReplaceAll(string(data), "tcpOnly=true,checkTCP=true,", ""))
	if err := os.WriteFile(cfg.Render.NodeDNSDist.Path, retained, 0644); err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	agent.state.Snapshot = &snapshot
	agent.state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	agent.state.Active = &activeConfiguration{Snapshot: snapshot, Files: map[string][]byte{cfg.Render.NodeDNSDist.Path: retained}}
	agent.state.RenderedHash = hashFiles(agent.state.Active.Files)
	if err := agent.store.Save(agent.state); err != nil {
		t.Fatal(err)
	}
	if err := prepareDNSDistRouter(cfg.Render.NodeDNSDist); err != nil {
		t.Fatal(err)
	}
	runtimeDir := t.TempDir()
	if err := restoreDaemonPermissions(cfg.StateDir, runtimeDir, []string{"coredns"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(runtimeDir, "allow-coredns")); err != nil {
		t.Fatalf("refreshed dnsdist process settings withheld daemon start permission: %v", err)
	}
	runner := &recordingRunner{}
	restarted, err := NewAgent(cfg, Dependencies{Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.restoreActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("restoration reloaded a running daemon with matching accepted routing: %v", runner.calls)
	}
	if !installedFilesMatch(restarted.state.Active.Files) || !bytes.Equal(restarted.state.Active.Files[cfg.Render.NodeDNSDist.Path], data) {
		t.Fatal("accepted checkpoint and current process configuration differ")
	}
}
