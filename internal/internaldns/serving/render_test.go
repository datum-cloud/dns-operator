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
	if !bindingReadyInConfig(string(r.Files[c.NodeDNSDist.Path]), "10.253.0.51") {
		t.Fatal("ready binding absent")
	}
	if strings.Contains(string(r.Files[c.NodeDNSDist.Path]), "LuaAction") {
		t.Fatal("dnsdist runs a script on the query path")
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
	for _, required := range []string{"checkName=\".\"", "useProxyProtocol=true", "tcpOnly=true", "checkTCP=true", "maxCheckFailures=3", "rise=2"} {
		if !strings.Contains(conf, required) {
			t.Fatalf("dnsdist config missing %q:\n%s", required, conf)
		}
	}
}

func TestDNSDistListenerCountUsesReusePort(t *testing.T) {
	t.Parallel()
	c, s := renderFixture(t)
	c.Role = RenderRegionalDNSDist
	c.ClusterDNSDist.ListenerCount = 2
	r, err := Render(c, s, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	conf := string(r.Files[c.ClusterDNSDist.Path])
	for _, required := range []string{
		`setLocal("0.0.0.0:53",{reusePort=true})`,
		`addLocal("0.0.0.0:53",{reusePort=true})`,
	} {
		if strings.Count(conf, required) != 1 {
			t.Fatalf("dnsdist config has %d copies of %q:\n%s", strings.Count(conf, required), required, conf)
		}
	}
	if strings.Contains(conf, `setLocal("0.0.0.0:53")`) {
		t.Fatal("multiple listeners left the first socket without SO_REUSEPORT")
	}
	defaultConfig := c
	defaultConfig.ClusterDNSDist.ListenerCount = 0
	defaultRendered, err := Render(defaultConfig, s, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(defaultRendered.Files[c.ClusterDNSDist.Path]), `setLocal("0.0.0.0:53")`) ||
		strings.Contains(string(defaultRendered.Files[c.ClusterDNSDist.Path]), "reusePort") {
		t.Fatal("default listener rendering changed")
	}
	if dnsdistTopology(r.Files[c.ClusterDNSDist.Path]) == dnsdistTopology(defaultRendered.Files[c.ClusterDNSDist.Path]) {
		t.Fatal("listener count change does not require a replacement member")
	}
	oneConfig := defaultConfig
	oneConfig.ClusterDNSDist.ListenerCount = 1
	oneRendered, err := Render(oneConfig, s, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	if dnsdistTopology(oneRendered.Files[c.ClusterDNSDist.Path]) != dnsdistTopology(defaultRendered.Files[c.ClusterDNSDist.Path]) {
		t.Fatal("explicit one listener differs from the default topology")
	}
}

func TestDNSDistRejectsInvalidListenerCount(t *testing.T) {
	t.Parallel()
	for _, count := range []int{-1, 65} {
		c, s := renderFixture(t)
		c.Role = RenderRegionalDNSDist
		c.ClusterDNSDist.ListenerCount = count
		if _, err := Render(c, s, nil); err == nil {
			t.Fatalf("listener count %d accepted", count)
		}
	}
}

func bindingReadyInConfig(config, address string) bool {
	tagged := `appendRule(destination("` + address + `"), SetTagAction("dns-context"`
	unready := `appendRule(destination("` + address + `"), SetTagAction("context-unready"`
	return strings.Contains(config, tagged) && !strings.Contains(config, unready)
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

func TestDNSDistIdentityOptionReplacesPerQueryProxyHeader(t *testing.T) {
	t.Parallel()
	c, s := renderFixture(t)
	c.Role = RenderNode
	c.NodeDNSDist.IdentityKeyFile = "/run/internal-dns/identity.key"
	c.NodeCoreDNS.IdentityKeyFile = c.NodeDNSDist.IdentityKeyFile
	r, err := Render(c, s, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	conf := string(r.Files[c.NodeDNSDist.Path])
	if strings.Contains(conf, "useProxyProtocol") {
		t.Fatal("identity option transport still sends a PROXY header per query")
	}
	for _, required := range []string{"tcpOnly=true", `io.open("/run/internal-dns/identity.key","r")`, `appendRule(destination("10.253.0.51"), SetEDNSOptionAction(65431, internalDNSIdentityKey.."10.253.0.51"))`} {
		if !strings.Contains(conf, required) {
			t.Fatalf("dnsdist config missing %q:\n%s", required, conf)
		}
	}
	if decodedCoreDNS(t, r.Files[c.NodeCoreDNS.Path]).IdentityKeyFile != c.NodeCoreDNS.IdentityKeyFile {
		t.Fatal("node CoreDNS does not read the identity key")
	}
	proxy, err := Render(RenderConfig{Role: RenderNode, NodeDNSDist: DNSDistRenderConfig{Path: c.NodeDNSDist.Path, ListenAddress: c.NodeDNSDist.ListenAddress, ACLs: c.NodeDNSDist.ACLs, PoolName: c.NodeDNSDist.PoolName, Backends: c.NodeDNSDist.Backends}, NodeCoreDNS: CoreDNSRenderConfig{Path: c.NodeCoreDNS.Path, ListenAddress: c.NodeCoreDNS.ListenAddress, Port: c.NodeCoreDNS.Port, ProxyPeers: c.NodeCoreDNS.ProxyPeers}}, s, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	if dnsdistTopology([]byte(conf)) == dnsdistTopology(proxy.Files[c.NodeDNSDist.Path]) {
		t.Fatal("changing the backend transport does not require a replacement member")
	}
}

func TestDNSDistUDPBackendRequiresKeyAndReplacement(t *testing.T) {
	t.Parallel()
	c, s := renderFixture(t)
	c.Role = RenderNode
	c.NodeDNSDist.BackendTransport = model.TransportUDP
	if _, err := Render(c, s, nil); err == nil {
		t.Fatal("UDP enabled without per-query identity")
	}
	c.NodeDNSDist.IdentityKeyFile = "/run/internal-dns/identity.key"
	c.NodeCoreDNS.IdentityKeyFile = c.NodeDNSDist.IdentityKeyFile
	r, err := Render(c, s, map[string]bool{"bind-a": true})
	if err != nil {
		t.Fatal(err)
	}
	conf := string(r.Files[c.NodeDNSDist.Path])
	for _, required := range []string{"tcpOnly=false", "checkTCP=false", "SetEDNSOptionAction(65431"} {
		if !strings.Contains(conf, required) {
			t.Fatalf("UDP configuration missing %q", required)
		}
	}
	if strings.Contains(conf, "useProxyProtocol") {
		t.Fatal("UDP still uses connection identity")
	}
	// The documented JSON spelling must work as well as model.TransportUDP,
	// whose value is uppercase because it is also used by Kubernetes APIs.
	if err := json.Unmarshal([]byte(`{"backendTransport":"udp"}`), &c.NodeDNSDist); err != nil {
		t.Fatal(err)
	}
	lower, err := Render(c, s, map[string]bool{"bind-a": true})
	if err != nil || string(lower.Files[c.NodeDNSDist.Path]) != conf {
		t.Fatalf("documented lowercase UDP changes rendering: %v", err)
	}
	c.NodeDNSDist.BackendTransport = model.TransportTCP
	tcp, err := Render(c, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dnsdistTopology(tcp.Files[c.NodeDNSDist.Path]) == dnsdistTopology(r.Files[c.NodeDNSDist.Path]) {
		t.Fatal("transport changed without requiring a replacement member")
	}
	c.NodeDNSDist.BackendTransport = "quic"
	if _, err := Render(c, s, nil); err == nil {
		t.Fatal("unsupported transport accepted")
	}
}

func TestNodeIdentityKeyMustMatchAcrossDaemons(t *testing.T) {
	t.Parallel()
	c, s := renderFixture(t)
	c.Role = RenderNode
	c.NodeDNSDist.IdentityKeyFile = "/run/internal-dns/identity.key"
	if _, err := Render(c, s, nil); err == nil {
		t.Fatal("node dnsdist sends identity options its CoreDNS cannot verify")
	}
	c.NodeCoreDNS.IdentityKeyFile = "identity.key"
	c.NodeDNSDist.IdentityKeyFile = "identity.key"
	if _, err := Render(c, s, nil); err == nil {
		t.Fatal("relative identity key path accepted")
	}
}

func TestRenderForwardsLoopbackReadinessCanaryBeforeClassifying(t *testing.T) {
	t.Parallel()
	c, s := renderFixture(t)
	for _, bindings := range [][]model.Binding{s.Bindings, nil} {
		s.Bindings = bindings
		r, err := Render(c, s, map[string]bool{"bind-a": true})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range []DNSDistRenderConfig{c.NodeDNSDist, c.ClusterDNSDist} {
			config := string(r.Files[d.Path])
			canary := `local function readinessCanary() local nmg = newNMG(); nmg:addMask("127.0.0.0/8"); nmg:addMask("::1/128"); return AndRule({NetmaskGroupRule(nmg, true, true),QClassRule(DNSClass.CHAOS),QTypeRule(DNSQType.TXT),QNameRule("ready.internal-dns.")}) end`
			unavailable := `appendRule(AndRule({readinessCanary(),NotRule(PoolAvailableRule("` + d.PoolName + `"))}),RCodeAction(DNSRCode.SERVFAIL))`
			forward := `appendRule(readinessCanary(),PoolAction("` + d.PoolName + `"))`
			refuse := `appendRule(NotRule(TagRule("dns-context")), RCodeAction(DNSRCode.REFUSED))`
			at := []int{strings.Index(config, canary), strings.Index(config, unavailable), strings.Index(config, forward), strings.Index(config, refuse)}
			for i := range at {
				if at[i] < 0 || i > 0 && at[i] < at[i-1] {
					t.Fatalf("%s with %d bindings must define the loopback canary, answer SERVFAIL without a backend, then forward, all before refusing unclassified traffic: %v", d.Path, len(bindings), at)
				}
			}
			if strings.Count(config, "readinessCanary()") != 3 {
				t.Fatalf("%s uses the canary rule outside its two loopback rules", d.Path)
			}
		}
	}
}

func TestRenderReadinessCanaryMatchesLocalProbeSourcesAndIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		listen string
		mask   string
	}{
		{"0.0.0.0:53", ""},
		{"[::]:53", ""},
		{"127.0.0.1:53", ""},
		{"10.253.0.5:53", `nmg:addMask("10.253.0.5/32");`},
		{"[fd71::5]:53", `nmg:addMask("fd71::5/128");`},
	} {
		canary := string(renderDNSDistReadinessCanary(DNSDistRenderConfig{ListenAddress: tc.listen, PoolName: "pool"}))
		if strings.Count(canary, "nmg:addMask(") != 2+min(len(tc.mask), 1) || tc.mask != "" && !strings.Contains(canary, tc.mask) {
			t.Fatalf("%s canary sources:\n%s", tc.listen, canary)
		}
		if strings.Contains(canary, "SetEDNSOptionAction") {
			t.Fatal("canary carries an identity option without an identity key")
		}
	}
	keyed := string(renderDNSDistReadinessCanary(DNSDistRenderConfig{ListenAddress: "[::]:53", PoolName: "pool", IdentityKeyFile: "/run/identity.key"}))
	identity := strings.Index(keyed, `appendRule(readinessCanary(),SetEDNSOptionAction(65431, internalDNSIdentityKey.."::1"))`)
	forward := strings.Index(keyed, `appendRule(readinessCanary(),PoolAction("pool"))`)
	if identity < 0 || forward < identity {
		t.Fatalf("keyed canary must carry the identity option to the pool:\n%s", keyed)
	}
}
