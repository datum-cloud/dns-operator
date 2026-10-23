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
	for _, address := range []string{"[::]:9153", "0.0.0.0:9153", "localhost:9153", "10.0.0.1", "10.0.0.1:0"} {
		if _, err := coreDNSArgs(CoreDNSRenderConfig{Path: "/etc/c.json", MetricsAddress: address}); err == nil {
			t.Errorf("metrics address %q was accepted", address)
		}
	}
}

func TestCoreDNSMetricsAddressAcceptsIPv6PodIP(t *testing.T) {
	t.Setenv("POD_IP", "fd00::7")
	for _, raw := range []string{"${POD_IP}:9153", "[${POD_IP}]:9153"} {
		args, err := coreDNSArgs(CoreDNSRenderConfig{Path: "/etc/c.json", MetricsAddress: raw})
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := args[len(args)-1]; got != "[fd00::7]:9153" {
			t.Fatalf("%s expanded to %q", raw, got)
		}
	}
}

func TestRenderConfigRejectsWildcardMetricsAddress(t *testing.T) {
	renderConfig, _ := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeCoreDNS.MetricsAddress = "[::]:9153"
	if err := validateRenderConfig(renderConfig); err == nil {
		t.Fatal("a wildcard metrics address passed validation")
	}
	renderConfig.NodeCoreDNS.MetricsAddress = "${POD_IP}:9153"
	if err := validateRenderConfig(renderConfig); err != nil {
		t.Fatalf("a pod IP reference failed validation: %v", err)
	}
}
