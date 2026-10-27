//go:build linux

// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"net"
	"testing"
	"time"
)

func TestUDPSocketKernelDropsAndIsolation(t *testing.T) {
	for _, network := range []string{"udp4", "udp6"} {
		t.Run(network, func(t *testing.T) {
			ip := net.IPv4(127, 0, 0, 1)
			if network == "udp6" {
				ip = net.IPv6loopback
			}
			listener, err := net.ListenUDP(network, &net.UDPAddr{IP: ip})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			if err := listener.SetReadBuffer(1024); err != nil {
				t.Fatal(err)
			}
			other, err := net.ListenUDP(network, &net.UDPAddr{IP: ip})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = other.Close() }()
			read, err := udpSocketStatsReader(listener)
			if err != nil {
				t.Fatal(err)
			}
			otherRead, err := udpSocketStatsReader(other)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := read(); err != nil || got != 0 {
				t.Fatalf("fresh socket drops=%d: %v", got, err)
			}
			sender, err := net.DialUDP(network, nil, listener.LocalAddr().(*net.UDPAddr))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sender.Close() }()
			if err := sender.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			// Do not read from this small-buffer listener. Local datagrams deliberately
			// overflow it without privileges or any change to namespace sysctls.
			payload := make([]byte, 512)
			for i := 0; i < 256; i++ {
				if _, err := sender.Write(payload); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.Now().Add(time.Second)
			var drops uint64
			for time.Now().Before(deadline) {
				drops, err = read()
				if err != nil {
					t.Fatal(err)
				}
				if drops > 0 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if drops == 0 {
				t.Fatal("kernel receive-buffer overflow not observed")
			}
			if got, err := otherRead(); err != nil || got != 0 {
				t.Fatalf("unrelated socket incorrectly attributed drops=%d: %v", got, err)
			}
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := read(); err == nil {
				t.Fatal("closed socket reported valid statistics")
			}
			t.Logf("observed %d kernel drops on %s, unrelated socket remains zero", drops, network)
		})
	}
}
