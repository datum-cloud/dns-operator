// SPDX-License-Identifier: AGPL-3.0-only
package serving

import (
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	contextdns "go.miloapis.com/dns-operator/internal/internaldns/coredns"
)

func TestDaemonShutdownHelper(t *testing.T) {
	if os.Getenv("DNS_DRAIN_HELPER") != "1" {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	if err := os.WriteFile(os.Getenv("DNS_DRAIN_READY"), nil, 0600); err != nil {
		os.Exit(2)
	}
	<-ch
	time.Sleep(1200 * time.Millisecond)
	if err := os.WriteFile(os.Getenv("DNS_DRAIN_FINISHED"), nil, 0600); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestDaemonStopAllowsDrainButGateRemainsBounded(t *testing.T) {
	for _, graceful := range []bool{true, false} {
		t.Run(map[bool]string{true: "shutdown", false: "fail-closed"}[graceful], func(t *testing.T) {
			dir := t.TempDir()
			ready := filepath.Join(dir, "ready")
			finished := filepath.Join(dir, "finished")
			t.Setenv("DNS_DRAIN_HELPER", "1")
			t.Setenv("DNS_DRAIN_READY", ready)
			t.Setenv("DNS_DRAIN_FINISHED", finished)
			p := &daemonProcess{name: os.Args[0], args: []string{"-test.run=^TestDaemonShutdownHelper$"}}
			if err := p.start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.stop() })
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("helper not ready")
				}
				time.Sleep(10 * time.Millisecond)
			}
			var err error
			if graceful {
				err = p.stopWithin(contextdns.ShutdownTimeout)
			} else {
				err = p.stop()
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = os.Stat(finished)
			if graceful && err != nil {
				t.Fatal("supervisor killed draining child")
			}
			if !graceful && !os.IsNotExist(err) {
				t.Fatal("fail-closed waited for drain")
			}
		})
	}
}
