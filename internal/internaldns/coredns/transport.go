// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

var proxySignature = []byte("\r\n\r\n\x00\r\nQUIT\n")

const maxProxyHandshakes = 64

type proxyListener struct {
	net.Listener
	peers     map[netip.Addr]bool
	once      sync.Once
	closeOnce sync.Once
	ready     chan net.Conn
	done      chan struct{}
}

func (l *proxyListener) start() {
	l.once.Do(func() {
		l.ready = make(chan net.Conn)
		l.done = make(chan struct{})
		go l.acceptConnections()
	})
}

func (l *proxyListener) Accept() (net.Conn, error) {
	l.start()
	select {
	case conn := <-l.ready:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *proxyListener) Close() error {
	l.start()
	var err error
	l.closeOnce.Do(func() {
		close(l.done)
		err = l.Listener.Close()
	})
	return err
}

func (l *proxyListener) acceptConnections() {
	pending := make(chan struct{}, maxProxyHandshakes)
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			_ = l.Close()
			return
		}
		if !trusted(l.peers, conn.RemoteAddr()) {
			_ = conn.Close()
			continue
		}
		select {
		case pending <- struct{}{}:
			go func() {
				defer func() { <-pending }()
				accepted, err := readProxyConnection(conn)
				if err != nil {
					_ = conn.Close()
					return
				}
				select {
				case l.ready <- accepted:
				case <-l.done:
					_ = conn.Close()
				}
			}()
		default:
			_ = conn.Close()
		}
	}
}

func readProxyConnection(conn net.Conn) (net.Conn, error) {
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return nil, err
	}
	header := make([]byte, 16)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	if !bytes.Equal(header[:12], proxySignature) {
		return nil, errors.New("invalid PROXY signature")
	}
	size := int(binary.BigEndian.Uint16(header[14:16]))
	if size > 4096 {
		return nil, errors.New("PROXY header exceeds size limit")
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, err
	}
	source, destination, _, err := parseHeader(append(header, body...), true)
	if err != nil {
		return nil, err
	}
	if source == nil {
		source, destination = conn.RemoteAddr(), conn.LocalAddr()
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return &proxyConn{Conn: conn, source: source, destination: destination}, nil
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
	if len(data) < 16 || !bytes.Equal(data[:12], proxySignature) || data[12] != 0x21 && data[12] != 0x20 {
		return nil, nil, 0, invalid
	}
	size := 16 + int(binary.BigEndian.Uint16(data[14:16]))
	if size > len(data) || size > 4112 {
		return nil, nil, 0, invalid
	}
	if data[12] == 0x20 {
		return nil, nil, size, nil
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
