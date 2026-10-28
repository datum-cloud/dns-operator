//go:build linux

// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"fmt"
	"net"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sys/unix"
)

func observeUDPSocket(packet net.PacketConn) (func(), error) {
	conn, ok := packet.(*net.UDPConn)
	if !ok {
		return nil, fmt.Errorf("UDP listener does not expose socket statistics")
	}
	read, err := udpSocketStatsReader(conn)
	if err != nil {
		return nil, err
	}
	collector := newUDPSocketCollector(read)
	if err := prometheus.Register(collector); err != nil {
		return nil, err
	}
	return func() { prometheus.Unregister(collector) }, nil
}

func udpSocketStatsReader(conn *net.UDPConn) (func() (uint64, error), error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	path := "/proc/net/udp"
	var socketErr error
	err = raw.Control(func(fd uintptr) {
		addr, err := unix.Getsockname(int(fd))
		socketErr = err
		if _, ok := addr.(*unix.SockaddrInet6); ok {
			path = "/proc/net/udp6"
		}
	})
	if err != nil {
		return nil, err
	}
	if socketErr != nil {
		return nil, socketErr
	}
	return func() (uint64, error) {
		// Recheck the live descriptor on every scrape; a closed listener must not
		// accidentally report a different socket that later reuses its inode.
		var stat unix.Stat_t
		var statErr error
		if err := raw.Control(func(fd uintptr) { statErr = unix.Fstat(int(fd), &stat) }); err != nil {
			return 0, err
		}
		if statErr != nil {
			return 0, statErr
		}
		file, err := os.Open(path)
		if err != nil {
			return 0, err
		}
		defer func() { _ = file.Close() }()
		return readUDPSocketDrops(file, stat.Ino)
	}, nil
}

func udpReadBufferSize(conn *net.UDPConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var size int
	var socketErr error
	err = raw.Control(func(fd uintptr) { size, socketErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF) })
	if err != nil {
		return 0, err
	}
	return size, socketErr
}
