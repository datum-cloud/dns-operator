// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	contextdns "go.miloapis.com/dns-operator/internal/internaldns/coredns"
)

func readinessCanaryReply(request *dns.Msg, rcode int, text string) *dns.Msg {
	response := new(dns.Msg)
	response.SetRcode(request, rcode)
	if rcode == dns.RcodeSuccess && text != "" && contextdns.IsReadinessCanary(request) {
		response.Answer = []dns.RR{&dns.TXT{Hdr: dns.RR_Header{Name: contextdns.ReadinessCanaryName, Rrtype: dns.TypeTXT, Class: dns.ClassCHAOS}, Txt: []string{text}}}
	}
	return response
}

type readinessFixture struct {
	dir        string
	configPath string
	files      map[string][]byte
	config     Config
	rcode      *atomic.Int32
	text       *atomic.Pointer[string]
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
	text := &atomic.Pointer[string]{}
	canary := contextdns.ReadinessCanaryText
	text.Store(&canary)
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		_ = w.WriteMsg(readinessCanaryReply(request, int(rcode.Load()), *text.Load()))
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
	return readinessFixture{dir: dir, configPath: configPath, files: files, config: config, rcode: rcode, text: text, udpServer: udpServer}
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
	f.rcode.Store(dns.RcodeSuccess)
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

func nodeReadinessConfig(t *testing.T, f readinessFixture, rcode int, text string) RenderConfig {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go serveProxiedTCPDNS(listener, rcode, text)
	return RenderConfig{Role: RenderNode, NodeDNSDist: f.config.Render.ClusterDNSDist, NodeCoreDNS: CoreDNSRenderConfig{ListenAddress: "127.0.0.1", Port: uint16(listener.Addr().(*net.TCPAddr).Port)}}
}

func TestReadinessRequiresTheCanaryAnswer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rcode     int
		text      string
		want      string
		frontPass bool
	}{
		{"canary answered", dns.RcodeSuccess, contextdns.ReadinessCanaryText, "", true},
		{"refused", dns.RcodeRefused, contextdns.ReadinessCanaryText, "returned REFUSED", true},
		{"daemon gated", dns.RcodeServerFailure, contextdns.ReadinessCanaryText, "returned SERVFAIL", false},
		{"name missing", dns.RcodeNameError, contextdns.ReadinessCanaryText, "returned NXDOMAIN", false},
		{"empty answer", dns.RcodeSuccess, "", "without a TXT answer", false},
		{"wrong text", dns.RcodeSuccess, "stale", `returned TXT ["stale"]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReadinessFixture(t)
			f.rcode.Store(int32(tc.rcode))
			f.text.Store(&tc.text)
			front := Readiness(context.Background(), f.config)
			if tc.frontPass != (front == nil) {
				t.Fatalf("regional front readiness = %v, want pass %v", front, tc.frontPass)
			}
			f.config.Render = nodeReadinessConfig(t, f, dns.RcodeSuccess, contextdns.ReadinessCanaryText)
			err := Readiness(context.Background(), f.config)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("a node answering the canary was not ready: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("node readiness error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestReadinessRequiresTheCoreDNSCanary(t *testing.T) {
	for _, rcode := range []int{dns.RcodeRefused, dns.RcodeServerFailure} {
		f := newReadinessFixture(t)
		f.rcode.Store(dns.RcodeSuccess)
		f.config.Render = nodeReadinessConfig(t, f, rcode, contextdns.ReadinessCanaryText)
		err := Readiness(context.Background(), f.config)
		if err == nil || !strings.Contains(err.Error(), "local CoreDNS readiness canary") || !strings.Contains(err.Error(), "returned "+dns.RcodeToString[rcode]) {
			t.Fatalf("CoreDNS answering %s was ready: %v", dns.RcodeToString[rcode], err)
		}
	}
}

func TestReadinessFailsWhenDNSDistPoolHasNoBackend(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backendServer := &dns.Server{Listener: backend, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		_ = w.WriteMsg(readinessCanaryReply(request, dns.RcodeSuccess, contextdns.ReadinessCanaryText))
	})}
	go func() { _ = backendServer.ActivateAndServe() }()
	t.Cleanup(func() { _ = backendServer.Shutdown() })
	f := newReadinessFixture(t)
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	frontUDP, err := net.ListenPacket("udp", front.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	forward := dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		answer, _, err := (&dns.Client{Net: "tcp", Timeout: 100 * time.Millisecond}).Exchange(request, backend.Addr().String())
		if err != nil {
			answer = new(dns.Msg)
			answer.SetRcode(request, dns.RcodeServerFailure)
		}
		_ = w.WriteMsg(answer)
	})
	frontTCP := &dns.Server{Listener: front, Handler: forward}
	frontPacket := &dns.Server{PacketConn: frontUDP, Handler: forward}
	go func() { _ = frontTCP.ActivateAndServe() }()
	go func() { _ = frontPacket.ActivateAndServe() }()
	t.Cleanup(func() { _ = frontTCP.Shutdown(); _ = frontPacket.Shutdown() })
	f.config.Render.ClusterDNSDist.ListenAddress = front.Addr().String()
	deadline := time.Now().Add(time.Second)
	for {
		err = Readiness(context.Background(), f.config)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dnsdist with a serving backend was not ready: %v", err)
	}
	if err := backendServer.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := Readiness(context.Background(), f.config); err == nil || !strings.Contains(err.Error(), "returned SERVFAIL") {
		t.Fatalf("dnsdist whose pool has no available backend was ready: %v", err)
	}
}

func TestReadinessNamesASlowDaemonWithinTheProbeTimeout(t *testing.T) {
	f := newReadinessFixture(t)
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	slow := dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		time.Sleep(900 * time.Millisecond)
		_ = w.WriteMsg(readinessCanaryReply(request, dns.RcodeSuccess, contextdns.ReadinessCanaryText))
	})
	udpServer := &dns.Server{PacketConn: udp, Handler: slow}
	tcpServer := &dns.Server{Listener: tcp, Handler: slow}
	go func() { _ = udpServer.ActivateAndServe() }()
	go func() { _ = tcpServer.ActivateAndServe() }()
	t.Cleanup(func() { _ = udpServer.Shutdown(); _ = tcpServer.Shutdown() })
	f.config.Render = nodeReadinessConfig(t, f, dns.RcodeSuccess, contextdns.ReadinessCanaryText)
	f.config.Render.NodeDNSDist.ListenAddress = tcp.Addr().String()
	started := time.Now()
	err = Readiness(context.Background(), f.config)
	if err == nil || !strings.Contains(err.Error(), "local dnsdist readiness canary "+tcp.Addr().String()) {
		t.Fatalf("answers slower than the probe timeout were ready or unnamed: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("readiness took %s, longer than the probe timeout", elapsed)
	}
}
