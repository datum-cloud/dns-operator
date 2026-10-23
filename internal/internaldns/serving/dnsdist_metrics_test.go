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
		"password=internalDNSWebSecret",
		"apiKey=internalDNSWebSecret",
		`io.open("/dev/urandom", "rb")`,
	} {
		if !strings.Contains(conf, required) {
			t.Fatalf("dnsdist config missing %q:\n%s", required, conf)
		}
	}
	init := strings.Index(conf, "if not internalDNSInitialized then")
	secret := strings.Index(conf, "internalDNSWebSecret = hashPassword(")
	server := strings.Index(conf, "webserver(address)")
	done := strings.Index(conf, "internalDNSInitialized = true")
	access := strings.Index(conf, "setWebserverConfig(")
	rules := strings.Index(conf, "local rules = {}")
	if init < 0 || secret < init || server < secret || done < server {
		t.Fatal("web server and its secret are not set up once inside the initialization block, so reloads would rebind it")
	}
	if !strings.Contains(conf[init:server], `if os.getenv("INTERNAL_DNS_VALIDATE") ~= "1" then`) {
		t.Fatal("configuration checks would try to start the web server")
	}
	if access < done || rules < access || !strings.Contains(conf[done:access], "if internalDNSWebSecret then") {
		t.Fatal("web server access is not reapplied on every reload, or is applied without the secret")
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
		{"${POD_IP}:8083", []string{"10.244.0.0/16"}, true},
		{"${POD_IP}", []string{"10.244.0.0/16"}, false},
		{"0.0.0.0:${PORT}", []string{"10.244.0.0/16"}, false},
		{"[::]:${PORT}", []string{"10.244.0.0/16"}, false},
		{"localhost:8083", []string{"10.244.0.0/16"}, false},
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
