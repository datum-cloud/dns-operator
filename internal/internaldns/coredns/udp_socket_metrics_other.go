//go:build !linux

// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import "net"

// Per-socket kernel drop statistics are currently supported only on Linux.
func observeUDPSocket(net.PacketConn) (func(), error) { return func() {}, nil }
