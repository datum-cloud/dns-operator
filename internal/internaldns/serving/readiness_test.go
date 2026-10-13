// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestReadinessUsesApprovedConfigurationWithoutAgentOrBroker(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg)
		response.SetRcode(request, dns.RcodeRefused)
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
	config := Config{StateDir: dir, Render: RenderConfig{Role: RenderRegionalDNSDist, ClusterDNSDist: DNSDistRenderConfig{ListenAddress: tcp.Addr().String()}}}
	if err := Readiness(context.Background(), config); err != nil {
		t.Fatalf("approved DNS required an agent heartbeat or delivery connection: %v", err)
	}
	if err := os.WriteFile(activationMarkerPath(dir), []byte("installing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Readiness(context.Background(), config); err == nil {
		t.Fatal("incomplete installation was ready")
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
