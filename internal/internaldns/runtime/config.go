package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/platform"
	"go.miloapis.com/dns-operator/internal/internaldns/serving"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
	"go.miloapis.com/dns-operator/internal/internaldns/watchdog"
	"k8s.io/apimachinery/pkg/types"
)

const (
	roleWatchdog       = "watchdog"
	roleControlPlane   = "control-plane"
	memberRoleRegional = "regional"
	roleAgent          = "agent"
	memberRoleCluster  = "cluster"
)

type ProjectConfig struct {
	Name             string              `json:"name,omitempty"`
	ProjectUID       types.UID           `json:"projectUID"`
	SourceClusterUID string              `json:"sourceClusterUID"`
	Namespace        string              `json:"namespace"`
	Kubeconfig       string              `json:"kubeconfig,omitempty"`
	KubeAPI          KubeAPIClientConfig `json:"kubeAPI,omitempty"`
}

type KubeAPIClientConfig struct {
	QPS                   float32 `json:"qps,omitempty"`
	Burst                 int     `json:"burst,omitempty"`
	RequestTimeoutSeconds int64   `json:"requestTimeoutSeconds,omitempty"`
}

type AdmissionConfig struct {
	Enabled                     bool     `json:"enabled"`
	Port                        int      `json:"port,omitempty"`
	CertDir                     string   `json:"certDir,omitempty"`
	PlatformSubjects            []string `json:"platformSubjects,omitempty"`
	MaxContributionLeaseSeconds int64    `json:"maxContributionLeaseSeconds,omitempty"`
	IntegrationSubjects         []string `json:"integrationSubjects,omitempty"`
	MaxAccessLeaseSeconds       int64    `json:"maxAccessLeaseSeconds,omitempty"`
}

type RegionalTransportConfig struct {
	Region       string                 `json:"region"`
	NATS         transport.Config       `json:"nats"`
	Stream       transport.StreamConfig `json:"stream"`
	EnsureStream bool                   `json:"ensureStream,omitempty"`
}

type Config struct {
	Region                string                    `json:"region"`
	Shard                 string                    `json:"shard"`
	PlatformNamespace     string                    `json:"platformNamespace"`
	PlatformKubeAPI       KubeAPIClientConfig       `json:"platformKubeAPI,omitempty"`
	Projects              []ProjectConfig           `json:"projects"`
	NodeBackends          []model.Backend           `json:"nodeBackends,omitempty"`
	ClusterBackends       []model.Backend           `json:"clusterBackends,omitempty"`
	Members               []model.Member            `json:"members,omitempty"`
	ConsumerPrefix        string                    `json:"consumerPrefix,omitempty"`
	ClusterPrefix         string                    `json:"clusterPrefix,omitempty"`
	NATS                  transport.Config          `json:"nats"`
	Stream                transport.StreamConfig    `json:"stream"`
	EnsureStream          bool                      `json:"ensureStream,omitempty"`
	RegionalTransports    []RegionalTransportConfig `json:"regionalTransports,omitempty"`
	Admission             AdmissionConfig           `json:"admission,omitempty"`
	OwnershipLeaseSeconds int64                     `json:"ownershipLeaseSeconds,omitempty"`
	ManagedDomainSuffix   string                    `json:"managedDomainSuffix,omitempty"`
	PrivateZoneClassName  string                    `json:"privateZoneClassName,omitempty"`
	Agent                 serving.Config            `json:"agent,omitempty"`
	Watchdog              watchdog.Config           `json:"watchdog,omitempty"`
	Shards                []platform.PlannerConfig  `json:"shards,omitempty"`
}

func LoadConfig(path string) (Config, error) {
	if path == "" {
		return Config{}, errors.New("--config is required")
	}
	if !filepath.IsAbs(path) {
		return Config{}, fmt.Errorf("config path must be absolute: %s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return Config{}, errors.New("config must contain exactly one JSON object")
		}
		return Config{}, fmt.Errorf("decode trailing config data: %w", err)
	}
	return c, nil
}

func (c *Config) defaults() {
	if c.PlatformNamespace == "" {
		c.PlatformNamespace = "internal-dns-system"
	}

	if c.OwnershipLeaseSeconds <= 0 {
		c.OwnershipLeaseSeconds = 30
	}
	if c.Admission.Port == 0 {
		c.Admission.Port = 9443
	}
	if c.Admission.MaxContributionLeaseSeconds <= 0 {
		c.Admission.MaxContributionLeaseSeconds = 90
	}
	if c.Admission.MaxAccessLeaseSeconds <= 0 {
		c.Admission.MaxAccessLeaseSeconds = 300
	}

	if c.NATS.Name == "" {
		c.NATS.Name = "internal-dns"
	}

	defaultKubeAPI(&c.PlatformKubeAPI, 50, 100)
	for i := range c.Projects {
		defaultKubeAPI(&c.Projects[i].KubeAPI, 20, 40)
	}
}

func defaultKubeAPI(c *KubeAPIClientConfig, qps float32, burst int) {
	if c.QPS == 0 {
		c.QPS = qps
	}
	if c.Burst == 0 {
		c.Burst = burst
	}
	if c.RequestTimeoutSeconds == 0 {
		c.RequestTimeoutSeconds = 10
	}
}

func validateKubeAPI(name string, c KubeAPIClientConfig) error {
	if c.QPS <= 0 || c.QPS > 1000 {
		return fmt.Errorf("%s.qps must be greater than 0 and at most 1000", name)
	}
	if c.Burst <= 0 || c.Burst > 2000 {
		return fmt.Errorf("%s.burst must be greater than 0 and at most 2000", name)
	}
	if c.RequestTimeoutSeconds <= 0 || c.RequestTimeoutSeconds > 120 {
		return fmt.Errorf("%s.requestTimeoutSeconds must be greater than 0 and at most 120", name)
	}
	return nil
}

func (c *Config) Validate(role string) error {
	c.defaults()
	switch role {
	case roleControlPlane, roleAgent, roleWatchdog:
	default:
		return fmt.Errorf("unsupported role %q", role)
	}
	if role == roleWatchdog {
		return c.Watchdog.Validate()
	}
	if len(c.RegionalTransports) == 0 {
		if err := validateTransportConfig(c.NATS, c.Stream, c.EnsureStream); err != nil {
			return err
		}
	} else {
		seen := map[string]bool{}
		for i := range c.RegionalTransports {
			regional := &c.RegionalTransports[i]
			if regional.NATS.Name == "" {
				regional.NATS.Name = "internal-dns-" + model.SafeToken(regional.Region)
			}
			if regional.Region == "" || seen[regional.Region] {
				return fmt.Errorf("regional transport %d has missing or duplicate region", i)
			}
			seen[regional.Region] = true
			if err := validateTransportConfig(regional.NATS, regional.Stream, regional.EnsureStream); err != nil {
				return fmt.Errorf("regional transport %s: %w", regional.Region, err)
			}
		}
	}
	if role == roleAgent {
		if c.Agent.Region == "" {
			c.Agent.Region = c.Region
		}
		if c.Agent.Shard == "" {
			c.Agent.Shard = c.Shard
		}
		if _, _, _, err := c.transportForRegion(c.Agent.Region); err != nil {
			return err
		}
		return c.Agent.Validate()
	}
	if c.PlatformNamespace == "" || len(c.Projects) == 0 {
		return errors.New("control-plane requires platformNamespace and projects")
	}
	if err := validateKubeAPI("platformKubeAPI", c.PlatformKubeAPI); err != nil {
		return err
	}
	names := map[string]bool{}
	uids := map[types.UID]bool{}
	namespaces := map[string]bool{}
	for i, p := range c.Projects {
		if p.ProjectUID == "" || p.SourceClusterUID == "" || p.Namespace == "" {
			return fmt.Errorf("project %d requires projectUID, sourceClusterUID, and namespace", i)
		}
		if p.Namespace == c.PlatformNamespace {
			return fmt.Errorf("project %d namespace collides with platformNamespace", i)
		}
		if err := validateKubeAPI(fmt.Sprintf("projects[%d].kubeAPI", i), p.KubeAPI); err != nil {
			return err
		}
		if uids[p.ProjectUID] {
			return fmt.Errorf("duplicate project UID %q", p.ProjectUID)
		}
		uids[p.ProjectUID] = true
		if p.Name != "" {
			if names[p.Name] {
				return fmt.Errorf("duplicate project name %q", p.Name)
			}
			names[p.Name] = true
		}
		if p.Kubeconfig == "" && namespaces[p.Namespace] {
			return fmt.Errorf("duplicate project namespace %q on the platform cluster", p.Namespace)
		}
		namespaces[p.Namespace] = true
	}
	if len(c.Projects) > 1 {
		for _, p := range c.Projects {
			if p.Name == "" {
				return errors.New("every project needs name when several projects are configured")
			}
		}
	}
	if c.Admission.Enabled && !filepath.IsAbs(c.Admission.CertDir) {
		return errors.New("admission.certDir must be absolute when admission is enabled")
	}
	if err := validateMembers(c); err != nil {
		return err
	}
	shardPairs := map[string]bool{}
	for i := range c.Shards {
		if c.Shards[i].Region == "" || c.Shards[i].Shard == "" {
			return fmt.Errorf("shard %d requires region and shard", i)
		}
		key := c.Shards[i].Region + "/" + c.Shards[i].Shard
		if shardPairs[key] {
			return fmt.Errorf("duplicate shard %q", key)
		}
		shardPairs[key] = true
	}
	for _, planner := range c.plannerConfigs() {
		if _, _, _, err := c.transportForRegion(planner.Region); err != nil {
			return err
		}
		count := 0
		for _, member := range planner.Members {
			if member.Role == memberRoleRegional || member.Role == memberRoleCluster {
				count++
			}
		}
		if count == 0 {
			return fmt.Errorf("shard %s/%s has %d regional resolver members; at least one is required", planner.Region, planner.Shard, count)
		}
	}
	return nil
}

func (c AdmissionConfig) maxAccessLease() time.Duration {
	return time.Duration(c.MaxAccessLeaseSeconds) * time.Second
}

func validateTransportConfig(nats transport.Config, stream transport.StreamConfig, ensure bool) error {
	if nats.URL == "" || stream.Name == "" {
		return errors.New("nats.url and stream.name are required")
	}
	if ensure && len(stream.Subjects) == 0 {
		return errors.New("stream.subjects are required when ensureStream is enabled")
	}
	if stream.SnapshotsBucket == "" {
		return errors.New("stream.snapshotsBucket is required for restart bootstrap")
	}
	return nil
}

func (c Config) transportForRegion(region string) (transport.Config, transport.StreamConfig, bool, error) {
	if len(c.RegionalTransports) == 0 {
		return c.NATS, c.Stream, c.EnsureStream, nil
	}
	for _, regional := range c.RegionalTransports {
		if regional.Region == region {
			return regional.NATS, regional.Stream, regional.EnsureStream, nil
		}
	}
	return transport.Config{}, transport.StreamConfig{}, false, fmt.Errorf("no transport configured for region %q", region)
}

func validateMembers(c *Config) error {
	seen := map[string]bool{}
	check := func(ms []model.Member) error {
		for _, m := range ms {
			if m.MemberID == "" || m.Role == "" {
				return errors.New("member ID and role are required")
			}
			if m.Role != "node" && m.Role != memberRoleRegional && m.Role != memberRoleCluster && m.Role != "resolver" {
				return fmt.Errorf("member %s has unsupported resolver role %s", m.MemberID, m.Role)
			}
			if seen[m.MemberID] {
				return fmt.Errorf("member ID %q is not globally unique", m.MemberID)
			}
			seen[m.MemberID] = true
		}
		return nil
	}
	if len(c.Shards) == 0 {
		return check(c.Members)
	}
	for _, s := range c.Shards {
		members := s.Members
		if len(members) == 0 {
			members = c.Members
		}
		if err := check(members); err != nil {
			return err
		}
	}
	return nil
}

func (c KubeAPIClientConfig) requestTimeout() time.Duration {
	return time.Duration(c.RequestTimeoutSeconds) * time.Second
}
func (c Config) ownershipLease() time.Duration {
	return time.Duration(c.OwnershipLeaseSeconds) * time.Second
}
func (c AdmissionConfig) maxLease() time.Duration {
	return time.Duration(c.MaxContributionLeaseSeconds) * time.Second
}
