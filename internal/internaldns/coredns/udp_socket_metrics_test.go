// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const udpSocketHeader = "sl local_address remote_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode ref pointer drops\n"

func TestReadUDPSocketDrops(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		want       uint64
		wantErr    bool
	}{
		{"IPv4", "0: 0100007F:14B4 00000000:0000 07 00000000:00000000 00:00000000 00000000 65532 0 42 2 0000000000000000 323\n", 323, false},
		{"IPv6", "0: 00000000000000000000000001000000:14B4 00000000000000000000000000000000:0000 07 00000000:00000000 00:00000000 00000000 65532 0 42 2 0000000000000000 374\n", 374, false},
		{"UnrelatedSamePort", "0: 0100007F:14B4 00000000:0000 07 0:0 00:0 0 65532 0 41 2 0 900\n1: 0100007F:14B4 00000000:0000 07 0:0 00:0 0 65532 0 42 2 0 0\n", 0, false},
		{"Missing", "0: 0100007F:14B4 00000000:0000 07 0:0 00:0 0 65532 0 41 2 0 900\n", 0, true},
		{"Truncated", "0: 0100007F:14B4 00000000:0000 07 0:0 00:0 0 65532 0 42 2 0\n", 0, true},
		{"InvalidDrops", "0: 0100007F:14B4 00000000:0000 07 0:0 00:0 0 65532 0 42 2 0 nope\n", 0, true},
		{"UnexpectedColumns", "0: 0100007F:14B4 00000000:0000 07 0:0 00:0 0 65532 0 42 2 0 323 extra\n", 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readUDPSocketDrops(strings.NewReader(udpSocketHeader+tt.body), 42)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("got(%d,%v), want(%d,error=%t)", got, err, tt.want, tt.wantErr)
			}
		})
	}
	if _, err := readUDPSocketDrops(errorUDPReader{}, 42); err == nil {
		t.Fatal("reader failure reported as success")
	}
}

type errorUDPReader struct{}

func (errorUDPReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestUDPSocketMetricsReadFailureAndRecovery(t *testing.T) {
	drops := uint64(323)
	var readErr error
	collector := newUDPSocketCollector(func() (uint64, error) { return drops, readErr })
	reg := prometheus.NewRegistry()
	reg.MustRegister(collector)
	expected := `# HELP internal_dns_transport_udp_socket_drops_total Kernel drops on the CoreDNS UDP listener since socket creation, including receive-buffer overflow. Resets when the listener is replaced.
# TYPE internal_dns_transport_udp_socket_drops_total counter
internal_dns_transport_udp_socket_drops_total 323
# HELP internal_dns_transport_udp_socket_stats_read_success Whether the CoreDNS UDP listener kernel statistics were read successfully on this scrape.
# TYPE internal_dns_transport_udp_socket_stats_read_success gauge
internal_dns_transport_udp_socket_stats_read_success 1
`
	if err := testutil.GatherAndCompare(tenantFreeGatherer{reg}, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
	readErr = errors.New("proc not readable")
	failed := `# HELP internal_dns_transport_udp_socket_stats_read_success Whether the CoreDNS UDP listener kernel statistics were read successfully on this scrape.
# TYPE internal_dns_transport_udp_socket_stats_read_success gauge
internal_dns_transport_udp_socket_stats_read_success 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(failed)); err != nil {
		t.Fatal(err)
	}
	readErr = nil
	drops = 374
	if err := testutil.GatherAndCompare(reg, strings.NewReader(strings.ReplaceAll(expected, " 323", " 374"))); err != nil {
		t.Fatal(err)
	}
	if !reg.Unregister(collector) {
		t.Fatal("collector could not unregister on socket shutdown")
	}
	if err := testutil.GatherAndCompare(reg, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
}
