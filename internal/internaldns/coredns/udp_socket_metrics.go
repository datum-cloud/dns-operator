// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// udpSocketCollector samples kernel state on scrape without changing packet
// reception. A failed read omits the drop counter rather than reporting zero.
type udpSocketCollector struct {
	read           func() (uint64, error)
	drops, success *prometheus.Desc
}

func newUDPSocketCollector(read func() (uint64, error)) *udpSocketCollector {
	return &udpSocketCollector{read: read,
		drops:   prometheus.NewDesc("internal_dns_transport_udp_socket_drops_total", "Kernel drops on the CoreDNS UDP listener since socket creation, including receive-buffer overflow. Resets when the listener is replaced.", nil, nil),
		success: prometheus.NewDesc("internal_dns_transport_udp_socket_stats_read_success", "Whether the CoreDNS UDP listener kernel statistics were read successfully on this scrape.", nil, nil),
	}
}

func (c *udpSocketCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.drops; ch <- c.success }
func (c *udpSocketCollector) Collect(ch chan<- prometheus.Metric) {
	drops, err := c.read()
	if err != nil {
		ch <- prometheus.MustNewConstMetric(c.success, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.success, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(c.drops, prometheus.CounterValue, float64(drops))
}

// readUDPSocketDrops selects the listener's inode, never another socket sharing
// its network namespace or port. Linux udp/udp6 rows combine queue/timer fields.
func readUDPSocketDrops(r io.Reader, inode uint64) (uint64, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}
		candidate, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil || candidate != inode {
			continue
		}
		if len(fields) != 13 {
			return 0, fmt.Errorf("unexpected UDP socket row with %d fields", len(fields))
		}
		drops, err := strconv.ParseUint(fields[12], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse UDP socket drops: %w", err)
		}
		return drops, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scan UDP socket table: %w", err)
	}
	return 0, fmt.Errorf("UDP listener inode not found")
}
