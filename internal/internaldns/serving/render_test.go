package serving

import (
	"strings"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func renderFixture(t *testing.T) (RenderConfig, model.ServingSnapshot) {
	t.Helper()
	now := time.Now()
	b := model.Binding{BindingUID: "bind-a", ProjectUID: "project-a", ContextUID: "vpc-a", BindingGeneration: 1, ConfigurationRevision: 3, ConsumerAddress: "10.253.0.51", ClusterAddress: "10.253.0.41", Port: 53, Transports: []model.Transport{model.TransportUDP, model.TransportTCP}, Zones: []model.ZoneAttachment{{ZoneUID: "zone-a", Apex: "prod.internal", RequiredPublicationEpoch: 1, RequiredPublicationRevision: 2}}, Authorization: model.Authorization{IssuerEpoch: 1, Revision: 2, ValidUntil: now.Add(time.Minute)}}
	c := RenderConfig{NodeDNSDist: DNSDistRenderConfig{Path: "/tmp/node-dnsdist.conf", ListenAddress: "0.0.0.0:53", ACLs: []string{"10.253.0.0/24"}, PoolName: "node-shared", Backends: []model.Backend{{MemberID: "node-bind-0", Address: "10.253.0.30", Port: 5300}}}, ClusterDNSDist: DNSDistRenderConfig{Path: "/tmp/cluster-dnsdist.conf", ListenAddress: "0.0.0.0:53", ACLs: []string{"10.253.0.0/24"}, PoolName: "cluster-shared", Backends: []model.Backend{{MemberID: "cluster-bind-0", Address: "10.253.0.20", Port: 5300}}}, NodeBIND: BINDRenderConfig{Path: "/tmp/node-bind.conf", ListenAddress: "10.253.0.30", Port: 5300, ProxyPeers: []string{"10.253.0.50"}, DefaultCacheSize: "16M"}, ClusterBIND: BINDRenderConfig{Path: "/tmp/cluster-bind.conf", ListenAddress: "10.253.0.20", Port: 5300, ProxyPeers: []string{"10.253.0.40"}, DefaultCacheSize: "16M"}}
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
	if !strings.Contains(string(r.Files[c.NodeBIND.Path]), "10.253.0.51") {
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

func TestRenderUsesOpaqueViewAndPrivatePROXYListener(t *testing.T) {
	t.Parallel()
	c, s := renderFixture(t)
	r, err := Render(c, s, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	cluster := string(r.Files[c.ClusterBIND.Path])
	if strings.Contains(cluster, "vpc-a") || !strings.Contains(cluster, "proxy plain") || strings.Contains(cluster, "query-source") || !strings.Contains(cluster, "forward only") {
		t.Fatalf("unsafe or incomplete cluster config:\n%s", cluster)
	}
	for path, config := range map[string]string{c.NodeBIND.Path: string(r.Files[c.NodeBIND.Path]), c.ClusterBIND.Path: cluster} {
		if !strings.Contains(config, `dnssec-validation auto;`) || !strings.Contains(config, `validate-except { "prod.internal"; };`) {
			t.Fatalf("%s does not retain public DNSSEC while excluding the private apex:\n%s", path, config)
		}
		if !strings.Contains(config, "max-ncache-ttl 5;") {
			t.Fatalf("%s does not bound private negative cache retention:\n%s", path, config)
		}
	}
}

func TestRenderConfiguresPrivateNegativeCacheBoundPerTier(t *testing.T) {
	t.Parallel()
	c, snapshot := renderFixture(t)
	c.NodeBIND.MaxNegativeCacheTTLSeconds = 3
	c.ClusterBIND.MaxNegativeCacheTTLSeconds = 7
	rendered, err := Render(c, snapshot, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	if config := string(rendered.Files[c.NodeBIND.Path]); !strings.Contains(config, "max-ncache-ttl 3;") {
		t.Fatalf("node view missing configured negative cache bound:\n%s", config)
	}
	if config := string(rendered.Files[c.ClusterBIND.Path]); !strings.Contains(config, "max-ncache-ttl 7;") {
		t.Fatalf("cluster view missing configured negative cache bound:\n%s", config)
	}
	c.NodeBIND.MaxNegativeCacheTTLSeconds = 61
	if _, err := Render(c, snapshot, map[string]bool{"bind-a": true}); err == nil {
		t.Fatal("unsafe negative cache retention was accepted")
	}
}

func TestRenderDNSSECExcludesEveryPrivateApexDeterministically(t *testing.T) {
	t.Parallel()
	c, snapshot := renderFixture(t)
	snapshot.Bindings[0].Zones = append(snapshot.Bindings[0].Zones,
		model.ZoneAttachment{ZoneUID: "zone-z", Apex: "z.private", RequiredPublicationEpoch: 1, RequiredPublicationRevision: 1},
		model.ZoneAttachment{ZoneUID: "zone-a2", Apex: "a.private", RequiredPublicationEpoch: 1, RequiredPublicationRevision: 1},
	)
	rendered, err := Render(c, snapshot, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	want := `validate-except { "a.private"; "prod.internal"; "z.private"; };`
	for _, path := range []string{c.NodeBIND.Path, c.ClusterBIND.Path} {
		if !strings.Contains(string(rendered.Files[path]), want) {
			t.Fatalf("%s missing sorted multi-zone DNSSEC exclusions:\n%s", path, rendered.Files[path])
		}
	}
}

func TestRenderRoleOwnsOnlyItsTier(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		role RenderRole
		want []string
		not  []string
	}{
		{RenderNode, []string{"/tmp/node-dnsdist.conf", "/tmp/node-bind.conf"}, []string{"/tmp/cluster-dnsdist.conf", "/tmp/cluster-bind.conf"}},
		{RenderCluster, []string{"/tmp/cluster-dnsdist.conf", "/tmp/cluster-bind.conf"}, []string{"/tmp/node-dnsdist.conf", "/tmp/node-bind.conf"}},
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

func TestDNSDistIncludesDeadlineAndBackendHealthCheck(t *testing.T) {
	t.Parallel()
	c, s := renderFixture(t)
	r, err := Render(c, s, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	conf := string(r.Files[c.NodeDNSDist.Path])
	for _, required := range []string{"validUntil=", "os.time() >= binding.validUntil", "checkName=\".\"", "useProxyProtocol=true"} {
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
