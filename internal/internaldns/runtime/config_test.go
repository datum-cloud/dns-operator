// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/serving"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
	"k8s.io/client-go/rest"
)

func TestAgentUsesRootRegionAndDoesNotNeedKubernetes(t *testing.T) {
	c := Config{
		Region: "us-central", Shard: "shared-1",
		NATS:   transport.Config{URL: "nats://fixture:4222", AllowInsecure: true},
		Stream: transport.StreamConfig{Name: "DNS", SnapshotsBucket: "DNS_BOOTSTRAP"},
		Agent: serving.Config{MemberID: "resolver-1", ReplicaID: "resolver-1", StateDir: t.TempDir(), Mode: serving.ModeResolver,
			Render: serving.RenderConfig{Role: serving.RenderNode,
				NodeDNSDist: serving.DNSDistRenderConfig{Path: t.TempDir() + "/dnsdist.conf", ListenAddress: "[::1]:53", PoolName: "node"},
				NodeBIND:    serving.BINDRenderConfig{Path: t.TempDir() + "/named.conf", ListenAddress: "::1", Port: 5353}},
			Transaction: serving.TransactionConfig{StageDir: t.TempDir()}, UnsafeAllowNoFailClosed: true, UnsafeAllowNoDNSVerify: true},
	}
	if err := c.Validate("agent"); err != nil {
		t.Fatal(err)
	}
	if c.Agent.Region != c.Region || c.Agent.Shard != c.Shard {
		t.Fatalf("agent did not inherit shard: %#v", c.Agent)
	}
}

func TestKubeAPIRateLimitDefaultsOverridesAndValidation(t *testing.T) {
	base := Config{
		Region: "central", Shard: "one",
		NATS:   transport.Config{URL: "nats://fixture:4222", AllowInsecure: true},
		Stream: transport.StreamConfig{Name: "DNS", SnapshotsBucket: "DNS_BOOTSTRAP"},
		Projects: []ProjectConfig{{Name: "project", ProjectUID: "p1", SourceClusterUID: "c1", Namespace: "project",
			KubeAPI: KubeAPIClientConfig{QPS: 30, Burst: 60}}},
		Members: []model.Member{{MemberID: "regional-1", Role: "regional"}},
	}
	if err := base.Validate("control-plane"); err != nil {
		t.Fatal(err)
	}
	if base.PlatformKubeAPI.QPS != 50 || base.PlatformKubeAPI.Burst != 100 {
		t.Fatalf("unexpected platform defaults: %#v", base.PlatformKubeAPI)
	}
	if base.Projects[0].KubeAPI.QPS != 30 || base.Projects[0].KubeAPI.Burst != 60 {
		t.Fatalf("source override was not preserved: %#v", base.Projects[0].KubeAPI)
	}
	restConfig := &rest.Config{}
	applyKubeAPILimit(restConfig, base.Projects[0].KubeAPI)
	if restConfig.QPS != 30 || restConfig.Burst != 60 || restConfig.RateLimiter == nil {
		t.Fatalf("REST client did not receive shared rate limiter: %#v", restConfig)
	}
	invalid := base
	invalid.PlatformKubeAPI.QPS = -1
	if err := invalid.Validate("control-plane"); err == nil {
		t.Fatal("negative platform Kubernetes API QPS was accepted")
	}
	invalid = base
	invalid.Projects = append([]ProjectConfig(nil), base.Projects...)
	invalid.Projects[0].KubeAPI.Burst = 2001
	if err := invalid.Validate("control-plane"); err == nil {
		t.Fatal("excessive source Kubernetes API burst was accepted")
	}
}

func TestProjectSourceFailureDoesNotStopHealthySource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failedStarted := make(chan struct{}, 1)
	healthyStarted := make(chan struct{}, 1)
	projects := []projectRuntime{
		{config: ProjectConfig{Name: "offline", ProjectUID: "p-offline"}, start: func(context.Context) error {
			failedStarted <- struct{}{}
			return errors.New("source API unavailable")
		}},
		{config: ProjectConfig{Name: "healthy", ProjectUID: "p-healthy"}, start: func(ctx context.Context) error {
			healthyStarted <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}},
	}
	startProjectSources(ctx, projects)
	for name, started := range map[string]<-chan struct{}{"offline": failedStarted, "healthy": healthyStarted} {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("%s source did not start independently", name)
		}
	}
}

func TestControlPlaneRejectsAmbiguousProjectsAndMemberIDs(t *testing.T) {
	base := Config{
		NATS:     transport.Config{URL: "nats://fixture:4222", AllowInsecure: true},
		Stream:   transport.StreamConfig{Name: "DNS", SnapshotsBucket: "DNS_BOOTSTRAP"},
		Projects: []ProjectConfig{{ProjectUID: "p1", SourceClusterUID: "c1", Namespace: "n1"}, {ProjectUID: "p2", SourceClusterUID: "c2", Namespace: "n2"}},
	}
	if err := base.Validate("control-plane"); err == nil {
		t.Fatal("unnamed multi-project configuration was accepted")
	}
	base.Projects[0].Name, base.Projects[1].Name = "one", "two"
	base.Members = []model.Member{{MemberID: "same", Role: "resolver"}, {MemberID: "same", Role: "regional"}}
	if err := base.Validate("control-plane"); err == nil {
		t.Fatal("duplicate fleet member identity was accepted")
	}
}

func TestConfigLoaderRejectsLegacyNetworkingIntegration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"networking":{"enabled":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("legacy DNS-owned networking integration was accepted")
	}
}

func TestConfigLoaderRejectsRemovedPrivateServingFields(t *testing.T) {
	for _, field := range []string{"authoritativeBackends", "sourcePrefix", "minAuthoritativeReplicas", "resolverContexts", "cacheSize", "authorizationTTLSeconds"} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte("{\""+field+"\":null}"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("removed field accepted: %v", err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{\"region\":\"central\",\"shard\":\"shared\"}"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil || loaded.Region != "central" {
		t.Fatalf("current config rejected: %#v %v", loaded, err)
	}
	if err := os.WriteFile(path, []byte("{} {}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("multiple config objects accepted")
	}
}
