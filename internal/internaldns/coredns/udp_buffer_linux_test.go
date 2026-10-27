//go:build linux

// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import "testing"

func TestUDPReadBufferAppliedToSocket(t *testing.T) {
	conn, err := listenUDP("127.0.0.1:0", 32768)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	size, err := udpReadBufferSize(conn)
	if err != nil {
		t.Fatal(err)
	}
	// Linux doubles SO_RCVBUF for socket accounting.
	if size < 65536 {
		t.Fatalf("receive buffer request not applied: %d", size)
	}
}
