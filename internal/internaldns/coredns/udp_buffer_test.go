// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestUDPReadBufferValidation(t *testing.T) {
	c, key, _ := keyedFixture(t)
	c.IdentityKeyFile = key
	c.UDPReadBufferBytes = -1
	if err := c.Validate(); err == nil {
		t.Fatal("negative buffer accepted")
	}
	c.UDPReadBufferBytes = 1024 * 1024
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.IdentityKeyFile = ""
	if err := c.Validate(); err == nil {
		t.Fatal("buffer accepted without UDP listener")
	}
	c.UDPReadBufferBytes = 0
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestUDPReadBufferChangeRequiresRestart(t *testing.T) {
	c, key, path := keyedFixture(t)
	c.IdentityKeyFile = key
	writeContexts(t, path, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path, "") }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if m, err := proxyQuery("tcp", c.ListenAddress, "fd53::a"); err == nil && address(m) == "192.0.2.10" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listener did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.UDPReadBufferBytes = 32768
	writeContexts(t, path, c)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "UDP read buffer") {
			t.Fatalf("expected replacement requirement, got %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("buffer change did not request restart")
	}
	conn, err := listenUDP(c.ListenAddress, c.UDPReadBufferBytes)
	if err != nil {
		t.Fatalf("old socket was not released: %v", err)
	}
	_ = conn.Close()
}
