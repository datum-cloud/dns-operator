// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"time"
)

var proxySignature = []byte("\r\n\r\n\x00\r\nQUIT\n")

type proxyAddress struct {
	peer, source, destination net.Addr
}

func (a *proxyAddress) Network() string               { return a.peer.Network() }
func (a *proxyAddress) String() string                { return a.source.String() }
func (a *proxyAddress) OriginalDestination() net.Addr { return a.destination }

type proxyPacketConn struct {
	net.PacketConn
	peers map[netip.Addr]bool
}

func (c *proxyPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, peer, err := c.PacketConn.ReadFrom(p)
		if err != nil {
			return n, peer, err
		}
		if !trusted(c.peers, peer) {
			continue
		}
		source, destination, size, err := parseHeader(p[:n], false)
		if err != nil || size >= n {
			continue
		}
		copy(p, p[size:n])
		return n - size, &proxyAddress{peer: peer, source: source, destination: destination}, nil
	}
}

func (c *proxyPacketConn) WriteTo(p []byte, peer net.Addr) (int, error) {
	if addr, ok := peer.(*proxyAddress); ok {
		peer = addr.peer
	}
	return c.PacketConn.WriteTo(p, peer)
}

type proxyListener struct {
	net.Listener
	peers map[netip.Addr]bool
}

func (l *proxyListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if !trusted(l.peers, conn.RemoteAddr()) {
			_ = conn.Close()
			continue
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		header := make([]byte, 16)
		if _, err := io.ReadFull(conn, header); err != nil || !bytes.Equal(header[:12], proxySignature) {
			_ = conn.Close()
			continue
		}
		size := int(binary.BigEndian.Uint16(header[14:16]))
		if size > 4096 {
			_ = conn.Close()
			continue
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(conn, body); err != nil {
			_ = conn.Close()
			continue
		}
		source, destination, _, err := parseHeader(append(header, body...), true)
		if err != nil {
			_ = conn.Close()
			continue
		}
		_ = conn.SetReadDeadline(time.Time{})
		return &proxyConn{Conn: conn, source: source, destination: destination}, nil
	}
}

type proxyConn struct {
	net.Conn
	source, destination net.Addr
}

func (c *proxyConn) RemoteAddr() net.Addr { return c.source }
func (c *proxyConn) LocalAddr() net.Addr  { return c.destination }

func trusted(peers map[netip.Addr]bool, addr net.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && peers[ip.Unmap()]
}

func parseHeader(data []byte, tcp bool) (net.Addr, net.Addr, int, error) {
	invalid := errors.New("a PROXYv2 address header for this transport is required")
	if len(data) < 16 || !bytes.Equal(data[:12], proxySignature) || data[12] != 0x21 {
		return nil, nil, 0, invalid
	}
	size := 16 + int(binary.BigEndian.Uint16(data[14:16]))
	if size > len(data) || size > 4112 {
		return nil, nil, 0, invalid
	}
	protocol := byte(2)
	if tcp {
		protocol = 1
	}
	if data[13]&0x0f != protocol {
		return nil, nil, 0, invalid
	}
	length := 4
	if data[13]>>4 == 2 {
		length = 16
	} else if data[13]>>4 != 1 {
		return nil, nil, 0, invalid
	}
	if size < 16+2*length+4 {
		return nil, nil, 0, invalid
	}
	source := net.IP(append([]byte(nil), data[16:16+length]...))
	destination := net.IP(append([]byte(nil), data[16+length:16+2*length]...))
	sourcePort := int(binary.BigEndian.Uint16(data[16+2*length:]))
	destinationPort := int(binary.BigEndian.Uint16(data[18+2*length:]))
	if tcp {
		return &net.TCPAddr{IP: source, Port: sourcePort}, &net.TCPAddr{IP: destination, Port: destinationPort}, size, nil
	}
	return &net.UDPAddr{IP: source, Port: sourcePort}, &net.UDPAddr{IP: destination, Port: destinationPort}, size, nil
}
