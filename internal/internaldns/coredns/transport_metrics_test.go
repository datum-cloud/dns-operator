// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func transportTestListener(t *testing.T, trustedPeer bool, plain ...bool) (*proxyListener, *transportMetrics, *prometheus.Registry) {
	t.Helper()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	m := newTransportMetrics(reg)
	l := &proxyListener{Listener: raw, peers: map[netip.Addr]bool{netip.MustParseAddr("127.0.0.1"): trustedPeer}, metrics: m}
	if len(plain) != 0 {
		l.plain = plain[0]
	}
	t.Cleanup(func() { _ = l.Close() })
	l.start()
	return l, m, reg
}

func transportTestDial(t *testing.T, l *proxyListener) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func transportWait(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("transport metric did not reach expected state")
}

func transportExpectClosed(t *testing.T, c net.Conn) {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := c.Read(b[:]); err == nil {
		t.Fatal("rejected connection remains readable")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("rejected connection was not closed")
	}
}

func TestTransportAdmissionMetricsLimitAndRecovery(t *testing.T) {
	l, m, reg := transportTestListener(t, true)
	stalled := make([]net.Conn, maxProxyHandshakes)
	for i := range stalled {
		stalled[i] = transportTestDial(t, l)
	}
	transportWait(t, func() bool { return testutil.ToFloat64(m.pending) == maxProxyHandshakes })
	rejected := transportTestDial(t, l)
	transportExpectClosed(t, rejected)
	transportWait(t, func() bool { return testutil.ToFloat64(m.connections.WithLabelValues(transportSaturated)) == 1 })
	if got := testutil.ToFloat64(m.pending); got != maxProxyHandshakes {
		t.Fatalf("pending=%v, want %d", got, maxProxyHandshakes)
	}

	// A stalled peer relinquishing its slot permits a valid connection through.
	_ = stalled[0].Close()
	transportWait(t, func() bool { return testutil.ToFloat64(m.pending) == maxProxyHandshakes-1 })
	valid := transportTestDial(t, l)
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	header := append(append([]byte(nil), proxySignature...), 0x20, 0, 0, 0)
	if _, err := valid.Write(append(header, []byte("query")...)); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-accepted:
		defer func() { _ = c.Close() }()
		if err := c.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		payload := make([]byte, 5)
		if _, err := io.ReadFull(c, payload); err != nil {
			t.Fatal(err)
		}
		if string(payload) != "query" {
			t.Fatalf("payload=%q", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("valid connection did not recover after capacity was released")
	}
	for _, c := range stalled {
		_ = c.Close()
	}
	transportWait(t, func() bool { return testutil.ToFloat64(m.pending) == 0 })
	if got := testutil.ToFloat64(m.connections.WithLabelValues(transportAccepted)); got != 1 {
		t.Fatalf("accepted=%v", got)
	}
	if got := testutil.ToFloat64(m.connections.WithLabelValues(transportHandshakeError)); got != maxProxyHandshakes {
		t.Fatalf("handshake errors=%v", got)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() != "outcome" {
					t.Fatalf("unbounded label: %s", label.GetName())
				}
			}
		}
	}
}

func TestTransportAdmissionMetricsRejectedPeersAndHeaders(t *testing.T) {
	for _, trustedPeer := range []bool{false, true} {
		name := "untrusted"
		outcome := transportUntrusted
		if trustedPeer {
			name = "invalid_header"
			outcome = transportHandshakeError
		}
		t.Run(name, func(t *testing.T) {
			l, m, _ := transportTestListener(t, trustedPeer)
			c := transportTestDial(t, l)
			if trustedPeer {
				if _, err := c.Write([]byte("invalid-head")); err != nil {
					t.Fatal(err)
				}
			}
			transportExpectClosed(t, c)
			transportWait(t, func() bool {
				return testutil.ToFloat64(m.connections.WithLabelValues(outcome)) == 1 && testutil.ToFloat64(m.pending) == 0
			})
			if got := testutil.ToFloat64(m.connections.WithLabelValues(transportAccepted)); got != 0 {
				t.Fatalf("rejected socket counted accepted: %v", got)
			}
		})
	}
}

func TestTransportAdmissionMetricsShutdownWhileWaitingForDNS(t *testing.T) {
	l, m, _ := transportTestListener(t, true)
	c := transportTestDial(t, l)
	header := append(append([]byte(nil), proxySignature...), 0x20, 0, 0, 0)
	if _, err := c.Write(header); err != nil {
		t.Fatal(err)
	}
	transportWait(t, func() bool { return testutil.ToFloat64(m.pending) == 1 })
	_ = l.Close()
	transportExpectClosed(t, c)
	transportWait(t, func() bool {
		return testutil.ToFloat64(m.connections.WithLabelValues(transportShutdown)) == 1 && testutil.ToFloat64(m.pending) == 0
	})
}

func TestTransportAdmissionMetricsPlainDNSPreservesPrefix(t *testing.T) {
	l, m, _ := transportTestListener(t, true, true)
	c := transportTestDial(t, l)
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := l.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	wire := []byte{0, 12, 0x12, 0x34, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if _, err := c.Write(wire); err != nil {
		t.Fatal(err)
	}
	select {
	case conn := <-accepted:
		defer func() { _ = conn.Close() }()
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(wire))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatal(err)
		}
		if string(got) != string(wire) {
			t.Fatalf("plain DNS changed: %x != %x", got, wire)
		}
	case <-time.After(time.Second):
		t.Fatal("plain DNS connection was not admitted")
	}
	transportWait(t, func() bool {
		return testutil.ToFloat64(m.connections.WithLabelValues(transportAccepted)) == 1 && testutil.ToFloat64(m.pending) == 0
	})
}
