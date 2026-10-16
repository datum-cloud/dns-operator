package serving

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	contextdns "go.miloapis.com/dns-operator/internal/internaldns/coredns"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func renderFixture(t *testing.T) (RenderConfig, model.ServingSnapshot) {
	t.Helper()
	now := time.Now()
	b := model.Binding{BindingUID: "bind-a", ProjectUID: "project-a", ContextUID: "vpc-a", BindingGeneration: 1, ConfigurationRevision: 3, ConsumerAddress: "10.253.0.51", ClusterAddress: "10.253.0.41", Port: 53, Transports: []model.Transport{model.TransportUDP, model.TransportTCP}, Zones: []model.ZoneAttachment{{ZoneUID: "zone-a", Apex: "prod.internal", RequiredPublicationEpoch: 1, RequiredPublicationRevision: 2}}, Version: model.AccessVersion{Epoch: 1, Sequence: 2}}
	c := RenderConfig{NodeDNSDist: DNSDistRenderConfig{Path: "/tmp/node-dnsdist.conf", ListenAddress: "0.0.0.0:53", ACLs: []string{"10.253.0.0/24"}, PoolName: "node-shared", Backends: []model.Backend{{MemberID: "node-coredns-0", Address: "10.253.0.30", Port: 5300}}}, ClusterDNSDist: DNSDistRenderConfig{Path: "/tmp/cluster-dnsdist.conf", ListenAddress: "0.0.0.0:53", ACLs: []string{"10.253.0.0/24"}, PoolName: "cluster-shared", Backends: []model.Backend{{MemberID: "cluster-coredns-0", Address: "10.253.0.20", Port: 5300}}}, NodeCoreDNS: CoreDNSRenderConfig{Path: "/tmp/node-coredns.json", ListenAddress: "10.253.0.30", Port: 5300, ProxyPeers: []string{"10.253.0.50"}}, ClusterCoreDNS: CoreDNSRenderConfig{Path: "/tmp/cluster-coredns.json", ListenAddress: "10.253.0.20", Port: 5300, ProxyPeers: []string{"10.253.0.40"}}}
	s := model.ServingSnapshot{Region: "east", Shard: "s1", ConfigurationEpoch: 1, ConfigurationRevision: 3, GeneratedAt: now, Bindings: []model.Binding{b}}
	return c, s
}

func TestRenderStagesViewsButActivatesOnlyReadyBindings(t *testing.T) {
	t.Parallel()
	c, s := renderFixture(t)
	r, err := Render(c, s, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if bindingReadyInConfig(string(r.Files[c.NodeDNSDist.Path]), "10.253.0.51") {
		t.Fatal("unready binding activated in dnsdist")
	}
	if !strings.Contains(string(r.Files[c.NodeCoreDNS.Path]), "10.253.0.51") {
		t.Fatal("view was not staged before activation")
	}
	r, err = Render(c, s, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r.Files[c.NodeDNSDist.Path]), "10.253.0.51") {
		t.Fatal("ready binding absent")
	}
	if strings.Contains(string(r.Files[c.NodeDNSDist.Path]), "newPacketCache") {
		t.Fatal("dnsdist packet cache enabled")
	}
}

func TestRenderCoreDNSPreservesContextAndPrivateZoneBoundary(t *testing.T) {
	t.Parallel()
	c, snapshot := renderFixture(t)
	r, err := Render(c, snapshot, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{c.NodeCoreDNS.Path, c.ClusterCoreDNS.Path} {
		cfg := decodedCoreDNS(t, r.Files[path])
		if len(cfg.ProxyPeers) != 1 || len(cfg.Contexts) != 1 || cfg.Contexts[0].ID != snapshot.Bindings[0].ViewName() {
			t.Fatalf("identity or proxy peers missing: %#v", cfg)
		}
		if len(cfg.Contexts[0].Zones) != 1 || cfg.Contexts[0].Zones[0].Apex != "prod.internal." {
			t.Fatal("private apex missing")
		}
	}
	cfg := decodedCoreDNS(t, r.Files[c.ClusterCoreDNS.Path])
	if !cfg.Contexts[0].Zones[0].Pending || cfg.Contexts[0].Zones[0].Data != "" {
		t.Fatal("missing private publication can be served")
	}
}

func TestRenderRejectsUntrustedCoreDNSListener(t *testing.T) {
	t.Parallel()
	c, snapshot := renderFixture(t)
	c.NodeCoreDNS.ProxyPeers = nil
	if _, err := Render(c, snapshot, nil); err == nil {
		t.Fatal("unprotected context listener accepted")
	}
}

func decodedCoreDNS(t *testing.T, data []byte) contextdns.Config {
	t.Helper()
	var cfg contextdns.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestRenderRoleOwnsOnlyItsTier(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		role RenderRole
		want []string
		not  []string
	}{
		{RenderNode, []string{"/tmp/node-dnsdist.conf", "/tmp/node-coredns.json"}, []string{"/tmp/cluster-dnsdist.conf", "/tmp/cluster-coredns.json"}},
		{RenderCluster, []string{"/tmp/cluster-dnsdist.conf", "/tmp/cluster-coredns.json"}, []string{"/tmp/node-dnsdist.conf", "/tmp/node-coredns.json"}},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			c, s := renderFixture(t)
			c.Role = tc.role
			r, err := Render(c, s, map[string]bool{"bind-a": true})
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range tc.want {
				if _, ok := r.Files[path]; !ok {
					t.Fatalf("role %s did not render %s", tc.role, path)
				}
			}
			for _, path := range tc.not {
				if _, ok := r.Files[path]; ok {
					t.Fatalf("role %s unexpectedly owns %s", tc.role, path)
				}
			}
		})
	}
}

func TestDNSDistUsesPersistentIdentityAndBackendHealthCheck(t *testing.T) {
	t.Parallel()
	c, s := renderFixture(t)
	r, err := Render(c, s, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	conf := string(r.Files[c.NodeDNSDist.Path])
	if strings.Contains(conf, "validUntil") || strings.Contains(conf, "os.time()") {
		t.Fatal("resolver access depends on a deadline")
	}
	for _, required := range []string{"checkName=\".\"", "useProxyProtocol=true"} {
		if !strings.Contains(conf, required) {
			t.Fatalf("dnsdist config missing %q:\n%s", required, conf)
		}
	}
}

func bindingReadyInConfig(config, address string) bool {
	for _, line := range strings.Split(config, "\n") {
		if strings.Contains(line, "[\""+address+"\"]") && strings.Contains(line, "ready=true") {
			return true
		}
	}
	return false
}

func TestPendingConfigurationCannotBeActivatedByReadinessMap(t *testing.T) {
	t.Parallel()
	config, snapshot := renderFixture(t)
	snapshot.Bindings[0].ConfigurationPending = true
	snapshot.Bindings[0].Zones = nil
	rendered, err := Render(config, snapshot, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{config.NodeDNSDist.Path, config.ClusterDNSDist.Path} {
		address := snapshot.Bindings[0].ConsumerAddress
		if path == config.ClusterDNSDist.Path {
			address = snapshot.Bindings[0].ClusterAddress
		}
		if bindingReadyInConfig(string(rendered.Files[path]), address) {
			t.Fatal("readiness activated a pending context")
		}
	}
	for _, path := range []string{config.NodeCoreDNS.Path, config.ClusterCoreDNS.Path} {
		if !decodedCoreDNS(t, rendered.Files[path]).Contexts[0].Pending {
			t.Fatal("pending CoreDNS context can recurse")
		}
	}
}
