// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

type readinessFixture struct {
	dir        string
	configPath string
	files      map[string][]byte
	config     Config
	rcode      *atomic.Int32
	udpServer  *dns.Server
}

func newReadinessFixture(t *testing.T) readinessFixture {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	rcode := &atomic.Int32{}
	rcode.Store(dns.RcodeRefused)
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg)
		response.SetRcode(request, int(rcode.Load()))
		_ = w.WriteMsg(response)
	})
	started := make(chan struct{}, 2)
	udpServer := &dns.Server{PacketConn: udp, Handler: handler, NotifyStartedFunc: func() { started <- struct{}{} }}
	tcpServer := &dns.Server{Listener: tcp, Handler: handler, NotifyStartedFunc: func() { started <- struct{}{} }}
	go func() { _ = udpServer.ActivateAndServe() }()
	go func() { _ = tcpServer.ActivateAndServe() }()
	t.Cleanup(func() { _ = udpServer.Shutdown(); _ = tcpServer.Shutdown() })
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("DNS fixture did not start")
		}
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "dnsdist.conf")
	files := map[string][]byte{configPath: []byte("approved configuration")}
	if err := os.WriteFile(configPath, files[configPath], 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot := testSnapshot(time.Now().Add(-24 * time.Hour))
	state := newCheckpoint()
	state.Snapshot = &snapshot
	state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	state.Active = &activeConfiguration{Snapshot: snapshot, Files: files}
	state.RenderedHash = hashFiles(files)
	if err := (stateStore{path: filepath.Join(dir, "checkpoint.json")}).Save(state); err != nil {
		t.Fatal(err)
	}
	if err := prepareServingState(dir); err != nil {
		t.Fatal(err)
	}
	config := Config{StateDir: dir, Render: RenderConfig{Role: RenderRegionalDNSDist, ClusterDNSDist: DNSDistRenderConfig{ListenAddress: tcp.Addr().String()}}}
	return readinessFixture{dir: dir, configPath: configPath, files: files, config: config, rcode: rcode, udpServer: udpServer}
}

func TestReadinessUsesApprovedConfigurationWithoutAgentOrBroker(t *testing.T) {
	f := newReadinessFixture(t)
	dir, configPath, files, config, udpServer := f.dir, f.configPath, f.files, f.config, f.udpServer
	if err := Readiness(context.Background(), config); err != nil {
		t.Fatalf("approved DNS required an agent heartbeat or delivery connection: %v", err)
	}
	if err := os.WriteFile(activationMarkerPath(dir), []byte("installing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Readiness(context.Background(), config); err == nil {
		t.Fatal("abandoned installation was ready")
	}
	if err := os.Remove(activationMarkerPath(dir)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("unapproved configuration"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Readiness(context.Background(), config); err == nil {
		t.Fatal("unapproved files were ready")
	}
	if err := os.WriteFile(configPath, files[configPath], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := udpServer.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := Readiness(context.Background(), config); err == nil {
		t.Fatal("TCP-only DNS was ready")
	}
}

func TestReadinessHoldsThroughRoutineInstallation(t *testing.T) {
	f := newReadinessFixture(t)
	tx := FileTransaction{MarkerPath: activationMarkerPath(f.dir)}
	if err := tx.beginActivation(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.configPath, []byte("candidate configuration"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Readiness(context.Background(), f.config); err != nil {
		t.Fatalf("installing an update made serving DNS unready: %v", err)
	}
	if err := tx.filesInstalled(); err != nil {
		t.Fatal(err)
	}
	if err := Readiness(context.Background(), f.config); err != nil {
		t.Fatalf("verifying an update made serving DNS unready: %v", err)
	}
	f.rcode.Store(dns.RcodeServerFailure)
	if err := Readiness(context.Background(), f.config); err == nil {
		t.Fatal("DNS answering SERVFAIL during installation was ready")
	}
	f.rcode.Store(dns.RcodeRefused)
	if err := tx.abandonActivation(); err != nil {
		t.Fatal(err)
	}
	if err := Readiness(context.Background(), f.config); err == nil || err.Error() != "configuration installation is incomplete" {
		t.Fatalf("an abandoned installation was ready: %v", err)
	}
}

func TestReadinessBoundsInstallationFromItsStart(t *testing.T) {
	f := newReadinessFixture(t)
	tx := FileTransaction{MarkerPath: activationMarkerPath(f.dir)}
	t.Cleanup(func() { _ = tx.abandonActivation() })
	if err := tx.beginActivation(); err != nil {
		t.Fatal(err)
	}
	started, err := readActivation(tx.MarkerPath)
	if err != nil {
		t.Fatal(err)
	}
	stale := activationState{Phase: activationVerifying, Since: time.Now().Add(-activationInstallGrace - time.Second)}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tx.MarkerPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := tx.beginActivation(); err != nil {
		t.Fatal(err)
	}
	if err := tx.filesInstalled(); err != nil {
		t.Fatal(err)
	}
	current, err := readActivation(tx.MarkerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Since.Equal(stale.Since) || current.Since.Equal(started.Since) {
		t.Fatalf("a later update restarted the installation clock: %v", current.Since)
	}
	if err := Readiness(context.Background(), f.config); err == nil {
		t.Fatal("updates that never committed kept the member ready past its grace period")
	}
	if err := tx.commitActivation(); err != nil {
		t.Fatal(err)
	}
	if err := tx.beginActivation(); err != nil {
		t.Fatal(err)
	}
	if err := Readiness(context.Background(), f.config); err != nil {
		t.Fatalf("a new installation after a commit inherited the old clock: %v", err)
	}
}

func TestReadinessSeesAbandonedInstallationThroughSupervisorLocking(t *testing.T) {
	f := newReadinessFixture(t)
	if err := os.WriteFile(activationMarkerPath(f.dir), []byte(`{"phase":"installing"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		_, _, release := daemonPermission(f.dir)
		released := make(chan struct{})
		time.AfterFunc(time.Millisecond, func() { release(); close(released) })
		err := Readiness(context.Background(), f.config)
		<-released
		if err == nil {
			t.Fatal("the supervisor's lock made an abandoned installation ready")
		}
	}
}

func TestReadinessHoldsAcrossInstallationCommits(t *testing.T) {
	f := newReadinessFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		tx := FileTransaction{MarkerPath: activationMarkerPath(f.dir)}
		for ctx.Err() == nil {
			if err := tx.beginActivation(); err != nil {
				done <- err
				return
			}
			if err := tx.commitActivation(); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	deadline := time.Now().Add(2 * time.Second)
	var failure error
	for time.Now().Before(deadline) && failure == nil {
		failure = Readiness(context.Background(), f.config)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if failure != nil {
		t.Fatalf("a committing installation made serving DNS unready: %v", failure)
	}
}

func TestReadinessRejectsGatedDaemon(t *testing.T) {
	f := newReadinessFixture(t)
	f.rcode.Store(dns.RcodeServerFailure)
	if err := Readiness(context.Background(), f.config); err == nil {
		t.Fatal("a daemon answering SERVFAIL was ready")
	}
}

func TestReadinessRejectsInstallationWithoutApprovedConfiguration(t *testing.T) {
	f := newReadinessFixture(t)
	state, err := (stateStore{path: filepath.Join(f.dir, "checkpoint.json")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	state.Active = nil
	if err := (stateStore{path: filepath.Join(f.dir, "checkpoint.json")}).Save(state); err != nil {
		t.Fatal(err)
	}
	tx := FileTransaction{MarkerPath: activationMarkerPath(f.dir)}
	if err := tx.beginActivation(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.abandonActivation() })
	if err := Readiness(context.Background(), f.config); err == nil {
		t.Fatal("an installation with no approved configuration was ready")
	}
}

func TestAgentRestartAndStopPreserveApprovedConfiguration(t *testing.T) {
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, time.Now().UTC())
	runner := &recordingRunner{}
	restarted, err := NewAgent(cfg, Dependencies{Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.restoreActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("agent restart changed healthy daemon configuration: %#v", runner.calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := restarted.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 || !installedFilesMatch(restarted.state.Active.Files) {
		t.Fatal("agent stop shut down healthy DNS")
	}
}

func TestFailClosedPersistsIncompleteActivationAcrossRestart(t *testing.T) {
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, time.Now().UTC())
	if err := agent.failClosed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !activationPending(cfg.StateDir) {
		t.Fatal("gated DNS must persist its incomplete activation")
	}
	restarted, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := Readiness(context.Background(), cfg); err == nil || err.Error() != "configuration installation is incomplete" {
		t.Fatalf("gated DNS must not report readiness after restart: %v", err)
	}
	if err := restarted.restoreActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if activationPending(cfg.StateDir) {
		t.Fatal("successful activation must clear the gate marker")
	}
}
