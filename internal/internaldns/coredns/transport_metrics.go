// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import "github.com/prometheus/client_golang/prometheus"

// Admission outcomes deliberately omit peer addresses and tenant identity.
const (
	transportAccepted       = "accepted"
	transportUntrusted      = "untrusted_peer"
	transportSaturated      = "handshake_limit"
	transportHandshakeError = "handshake_error"
	transportShutdown       = "shutdown"
)

type transportMetrics struct {
	connections        *prometheus.CounterVec
	pending            prometheus.Gauge
	datagramsAccepted  prometheus.Counter
	datagramsUntrusted prometheus.Counter
}

func newTransportMetrics(reg prometheus.Registerer) *transportMetrics {
	m := &transportMetrics{
		connections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "internal_dns_transport_connections_total",
			Help: "Accepted TCP sockets by transport admission outcome, before DNS query processing.",
		}, []string{"outcome"}),
		pending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "internal_dns_transport_pending_handshakes",
			Help: "TCP sockets reading their initial header or waiting for the DNS server to accept them.",
		}),
	}
	datagrams := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "internal_dns_transport_datagrams_total",
		Help: "UDP datagrams by peer admission outcome, before DNS parsing and identity validation.",
	}, []string{"outcome"})
	m.datagramsAccepted = datagrams.WithLabelValues(transportAccepted)
	m.datagramsUntrusted = datagrams.WithLabelValues(transportUntrusted)
	reg.MustRegister(datagrams)
	for _, outcome := range []string{transportAccepted, transportUntrusted, transportSaturated, transportHandshakeError, transportShutdown} {
		m.connections.WithLabelValues(outcome)
	}
	reg.MustRegister(m.connections, m.pending, prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "internal_dns_transport_handshake_limit",
		Help: "Maximum concurrent transport handshakes admitted by a listener.",
	}, func() float64 { return maxProxyHandshakes }))
	return m
}

var defaultTransportMetrics = newTransportMetrics(prometheus.DefaultRegisterer)
