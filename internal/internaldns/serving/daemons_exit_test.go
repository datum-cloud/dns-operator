// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"bytes"
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
