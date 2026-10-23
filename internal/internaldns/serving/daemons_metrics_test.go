// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"slices"
	"testing"
)

func TestCoreDNSMetricsAddressExpandsPodIP(t *testing.T) {
	t.Setenv("POD_IP", "10.244.1.7")
	args, err := coreDNSArgs(CoreDNSRenderConfig{Path: "/etc/c.json", MetricsAddress: "${POD_IP}:9153"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"-config", "/etc/c.json", "-metrics-address", "10.244.1.7:9153"}; !slices.Equal(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	args, err = coreDNSArgs(CoreDNSRenderConfig{Path: "/etc/c.json"})
	if err != nil || !slices.Equal(args, []string{"-config", "/etc/c.json"}) {
		t.Fatalf("metrics enabled without an address: %v %v", args, err)
	}
}

func TestCoreDNSMetricsAddressRejectsUnsetPodIP(t *testing.T) {
	t.Setenv("POD_IP", "")
	if _, err := coreDNSArgs(CoreDNSRenderConfig{Path: "/etc/c.json", MetricsAddress: "${POD_IP}:9153"}); err == nil {
		t.Fatal("a metrics address with no host was accepted, which would listen on every interface")
	}
	if _, err := coreDNSArgs(CoreDNSRenderConfig{Path: "/etc/c.json", MetricsAddress: "[::]:9153"}); err == nil {
		t.Fatal("an unspecified metrics address was accepted, which would listen on the producer network")
	}
}
