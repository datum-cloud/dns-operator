//go:build linux

// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import "testing"

func TestUDPReadBufferAppliedToSocket(t *testing.T) {
	baseline, err := listenUDP("127.0.0.1:0", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = baseline.Close() }()
	defaultSize, err := udpReadBufferSize(baseline)
	if err != nil {
		t.Fatal(err)
	}
	// Use a small request and ensure the expected result differs from the
	// default, so removing SetReadBuffer cannot make this test pass.
	requested := 16384
	if defaultSize == 2*requested {
		requested /= 2
	}
	conn, err := listenUDP("127.0.0.1:0", requested)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	size, err := udpReadBufferSize(conn)
	if err != nil {
		t.Fatal(err)
	}
	// Linux doubles SO_RCVBUF for socket accounting.
	if size != 2*requested {
		t.Fatalf("receive buffer request not applied: got %d, want %d (default %d)", size, 2*requested, defaultSize)
	}
}
