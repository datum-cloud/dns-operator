// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func captureDaemonLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := daemonLog
	daemonLog = &buf
	t.Cleanup(func() { daemonLog = previous })
	return &buf
}

func waitExited(t *testing.T, p *daemonProcess) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !p.reap() {
		if time.Now().After(deadline) {
			t.Fatal("process did not exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUnexpectedDaemonExitIsLoggedWithStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   string
	}{
		{name: "exit code", script: "exit 3", want: "DNS process sh exited unexpectedly (exit status 3)"},
		{name: "clean exit", script: "exit 0", want: "DNS process sh exited unexpectedly (exit status 0)"},
		{name: "signal", script: "kill -KILL $$", want: "DNS process sh exited unexpectedly (signal: killed)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := captureDaemonLog(t)
			p := &daemonProcess{name: "sh", args: []string{"-c", tc.script}}
			if err := p.start(); err != nil {
				t.Fatal(err)
			}
			waitExited(t, p)
			if got := strings.TrimSpace(log.String()); got != tc.want {
				t.Fatalf("log = %q, want %q", got, tc.want)
			}
			if p.cmd != nil || p.done != nil {
				t.Fatal("exited process was not cleared for restart")
			}
		})
	}
}

func TestPlannedDaemonStopLogsNothing(t *testing.T) {
	log := captureDaemonLog(t)
	p := &daemonProcess{name: "sleep", args: []string{"60"}}
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	if log.Len() != 0 {
		t.Fatalf("planned stop logged %q", log.String())
	}
}

func TestStopAfterCrashLogsTheCrash(t *testing.T) {
	log := captureDaemonLog(t)
	p := &daemonProcess{name: "sh", args: []string{"-c", "exit 7"}}
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	// Wait for the exit to be collected without consuming it.
	deadline := time.Now().Add(5 * time.Second)
	for len(p.done) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("process did not exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := p.stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "exited unexpectedly (exit status 7)") {
		t.Fatalf("log = %q, want the crash recorded", log.String())
	}
}

func TestGateStopIsNotLoggedAsACrash(t *testing.T) {
	log := captureDaemonLog(t)
	allow := filepath.Join(t.TempDir(), "allow-sh")
	if err := os.WriteFile(allow, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p := &daemonProcess{name: "sh", args: []string{"-c", "sleep 60"}, allowFile: allow}
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	// The gate removes the start permission, then kills the process.
	if err := os.Remove(allow); err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitExited(t, p)
	if log.Len() != 0 {
		t.Fatalf("gate stop logged %q", log.String())
	}
}

func TestRepeatedIdenticalExitsAreSummarized(t *testing.T) {
	log := captureDaemonLog(t)
	var exits exitLog
	start := time.Unix(1000, 0)
	for i := range 20 {
		exits.record("coredns", "exit status 1", start.Add(time.Duration(i)*50*time.Millisecond))
	}
	exits.record("coredns", "signal: killed", start.Add(2*time.Second))
	for i := range 3 {
		exits.record("coredns", "signal: killed", start.Add(3*time.Second+time.Duration(i)*time.Second))
	}
	exits.record("coredns", "signal: killed", start.Add(2*time.Minute))
	want := []string{
		"DNS process coredns exited unexpectedly (exit status 1)",
		"DNS process coredns exited unexpectedly (exit status 1) 19 more times before this",
		"DNS process coredns exited unexpectedly (signal: killed)",
		"DNS process coredns exited unexpectedly (signal: killed); 3 more times since the last report",
	}
	if got := strings.Split(strings.TrimSpace(log.String()), "\n"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("log =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
