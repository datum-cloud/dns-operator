// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"strings"
	"testing"
)

func TestDNSDistMetricsOffUnlessConfigured(t *testing.T) {
	t.Parallel()
	c, s := renderFixture(t)
	r, err := Render(c, s, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	if conf := string(r.Files[c.NodeDNSDist.Path]); strings.Contains(conf, "webserver(") {
		t.Fatalf("dnsdist web server enabled without a metrics address:\n%s", conf)
	}
}

func TestDNSDistMetricsStartOnceWithStatsOnly(t *testing.T) {
	t.Parallel()
	c, s := renderFixture(t)
	c.NodeDNSDist.MetricsAddress = "[${POD_IP}]:8083"
	c.NodeDNSDist.MetricsACLs = []string{"10.244.0.0/16", "fd20::/48"}
	r, err := Render(c, s, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	conf := string(r.Files[c.NodeDNSDist.Path])
	for _, required := range []string{
		`internalDNSMetricsAddress("[${POD_IP}]:8083")`,
		`acl="10.244.0.0/16,fd20::/48"`,
		"statsRequireAuthentication=false",
		"apiRequiresAuthentication=true",
		"dashboardRequiresAuthentication=true",
	} {
		if !strings.Contains(conf, required) {
			t.Fatalf("dnsdist config missing %q:\n%s", required, conf)
		}
	}
	if strings.Contains(conf, "password=") || strings.Contains(conf, "apiKey=") {
		t.Fatal("dnsdist web server credentials rendered; the API and dashboard must stay closed")
	}
	init := strings.Index(conf, "if not internalDNSInitialized then")
	server := strings.Index(conf, "webserver(address)")
	done := strings.Index(conf, "internalDNSInitialized = true")
	if init < 0 || server < init || done < server {
		t.Fatal("web server is not started inside the one-time initialization block, so reloads would rebind it")
	}
	if !strings.Contains(conf[init:server], `if os.getenv("INTERNAL_DNS_VALIDATE") ~= "1" then`) {
		t.Fatal("configuration checks would try to start the web server")
	}
	if strings.Contains(conf[:server], "local rules = {}") {
		t.Fatal("web server is configured in the reloadable rules section")
	}
}

func TestDNSDistMetricsKeepRetainedRulesAcrossAddressChange(t *testing.T) {
	c, _ := renderFixture(t)
	c.NodeDNSDist.MetricsAddress = "[${POD_IP}]:8083"
	c.NodeDNSDist.MetricsACLs = []string{"10.244.0.0/16"}
	before := renderDNSDist(c.NodeDNSDist, c.NodeDNSDist.Backends, nil, nil, false, nil, nil)
	c.NodeDNSDist.MetricsAddress = "[${POD_IP}]:9083"
	after := renderDNSDist(c.NodeDNSDist, c.NodeDNSDist.Backends, nil, nil, false, nil, nil)
	if dnsdistTopology(before) != dnsdistTopology(after) {
		t.Fatal("changing the metrics address changes the topology, so a restart would drop retained rules")
	}
}

func TestDNSDistMetricsConfigValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		address string
		acls    []string
		ok      bool
	}{
		{"", nil, true},
		{"[${POD_IP}]:8083", []string{"10.244.0.0/16"}, true},
		{"10.244.1.7:8083", []string{"10.244.0.0/16"}, true},
		{"", []string{"10.244.0.0/16"}, false},
		{"[${POD_IP}]:8083", nil, false},
		{"[::]:8083", []string{"10.244.0.0/16"}, false},
		{"0.0.0.0:8083", []string{"10.244.0.0/16"}, false},
		{"10.244.1.7", []string{"10.244.0.0/16"}, false},
		{"10.244.1.7:8083", []string{"0.0.0.0/0"}, false},
		{"10.244.1.7:8083", []string{"::/0"}, false},
		{"10.244.1.7:8083", []string{"pods"}, false},
	}
	for _, tc := range cases {
		c, _ := renderFixture(t)
		c.Role = RenderNode
		c.NodeDNSDist.MetricsAddress = tc.address
		c.NodeDNSDist.MetricsACLs = tc.acls
		if err := validateRenderConfig(c); (err == nil) != tc.ok {
			t.Errorf("address %q ACLs %v: err = %v, want ok = %v", tc.address, tc.acls, err, tc.ok)
		}
	}
}
