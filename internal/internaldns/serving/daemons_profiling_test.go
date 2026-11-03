// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"slices"
	"testing"
)

func TestCoreDNSProfileArguments(t *testing.T) {
	for _, metrics := range []string{"", "127.0.0.1:9153"} {
		args, err := coreDNSArgs(CoreDNSRenderConfig{Path: "/etc/c.json", MetricsAddress: metrics, ProfileAddress: "127.0.0.1:6060"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(args[len(args)-2:], []string{"-profile-address", "127.0.0.1:6060"}) {
			t.Fatalf("profiling flag absent: %v", args)
		}
	}
	args, err := coreDNSArgs(CoreDNSRenderConfig{Path: "/etc/c.json"})
	if err != nil || slices.Contains(args, "-profile-address") {
		t.Fatalf("profiling enabled by default: %v %v", args, err)
	}
	for _, address := range []string{"0.0.0.0:6060", "[::]:6060", "10.1.2.3:6060", "${POD_IP}:6060"} {
		if _, err := coreDNSArgs(CoreDNSRenderConfig{ProfileAddress: address}); err == nil {
			t.Errorf("daemon accepts %q", address)
		}
		cfg, _ := renderFixture(t)
		cfg.Role = RenderNode
		cfg.NodeCoreDNS.ProfileAddress = address
		if err := validateRenderConfig(cfg); err == nil {
			t.Errorf("render accepts %q", address)
		}
	}
	cfg, _ := renderFixture(t)
	cfg.Role = RenderNode
	cfg.NodeCoreDNS.ProfileAddress = "127.0.0.1:6060"
	if err := validateRenderConfig(cfg); err != nil {
		t.Fatal(err)
	}
}
