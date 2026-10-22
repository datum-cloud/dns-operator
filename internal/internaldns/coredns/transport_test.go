// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestSlowProxyHeaderDoesNotBlockOtherConnections(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &proxyListener{Listener: listener, peers: map[netip.Addr]bool{netip.MustParseAddr("127.0.0.1"): true}}
	t.Cleanup(func() { _ = proxy.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := proxy.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	stalled, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stalled.Close() }()
	if _, err := stalled.Write(proxySignature[:1]); err != nil {
		t.Fatal(err)
	}
	valid, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = valid.Close() }()
	header := append(append([]byte(nil), proxySignature...), 0x20, 0, 0, 0)
	if _, err := valid.Write(header); err != nil {
		t.Fatal(err)
	}
	select {
	case conn := <-accepted:
		_ = conn.Close()
	case <-time.After(500 * time.Millisecond):
		t.Fatal("a stalled header blocked another proxy connection")
	}
}
