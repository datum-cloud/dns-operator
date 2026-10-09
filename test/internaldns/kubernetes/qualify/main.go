// SPDX-License-Identifier: AGPL-3.0-only

// Command qualify publishes through project credentials and queries the shared
// Kubernetes serving fleet. The same executable runs DNS probes inside a pod.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"
	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type project struct {
	Name                                string `json:"name"`
	ProjectUID                          string `json:"projectUID"`
	SourceClusterUID                    string `json:"sourceClusterUID"`
	Namespace                           string `json:"namespace"`
	AdminKubeconfig                     string `json:"adminKubeconfig"`
	ProductKubeconfig                   string `json:"productKubeconfig"`
	IssuerKubeconfig                    string `json:"issuerKubeconfig"`
	IntegrationKubeconfig               string `json:"integrationKubeconfig"`
	PublisherSubject                    string `json:"publisherSubject"`
	ConsumerAddress                     string `json:"consumerAddress"`
	admin, product, issuer, integration client.Client
	resolverContext                     dnsv1.DNSResolverContext
	zone                                dnsv1.DNSZone
	access                              dnsv1.DNSResolverAccessBinding
}

type environment struct {
	PlatformKubeconfig    string    `json:"platformKubeconfig"`
	Namespace             string    `json:"namespace"`
	Region                string    `json:"region"`
	Shard                 string    `json:"shard"`
	ProbePod              string    `json:"probePod"`
	ProbeContainer        string    `json:"probeContainer"`
	ProbeBinary           string    `json:"probeBinary"`
	ControllerDeployments []string  `json:"controllerDeployments"`
	BrokerStatefulSet     string    `json:"brokerStatefulSet"`
	RegionalMembers       []string  `json:"regionalMembers"`
	Projects              []project `json:"projects"`
}

type check struct {
	Name     string    `json:"name"`
	Passed   bool      `json:"passed"`
	Time     time.Time `json:"time"`
	Evidence any       `json:"evidence,omitempty"`
	Error    string    `json:"error,omitempty"`
}

type report struct {
	Suite       string            `json:"suite"`
	Started     time.Time         `json:"started"`
	Completed   time.Time         `json:"completed"`
	Passed      bool              `json:"passed"`
	Checks      []check           `json:"checks"`
	Images      map[string]string `json:"images"`
	Limitations []string          `json:"limitations"`
	Error       string            `json:"error,omitempty"`
}

type probeResult struct {
	Rcode     int      `json:"rcode"`
	Answers   []string `json:"answers"`
	Addresses []string `json:"addresses"`
	TTL       uint32   `json:"ttl"`
	Error     string   `json:"error,omitempty"`
}

type publication struct {
	registration dnsv1.DNSRegistration
	grant        dnsv1.DNSContributionGrant
	contribution dnsv1.DNSRecordContribution
	sequence     int64
}

type suite struct {
	env      environment
	platform client.Client
	report   report
	ctx      context.Context
}

func main() {
	envPath := flag.String("environment", "", "Generated environment JSON")
	results := flag.String("results", "test/internaldns/results/kubernetes.json", "Qualification report")
	probe := flag.Bool("probe", false, "Run one DNS query from a workload pod")
	server := flag.String("server", "", "DNS server address and port")
	name := flag.String("name", "", "DNS query name")
	rrType := flag.String("type", "A", "DNS record type")
	tcp := flag.Bool("tcp", false, "Use DNS over TCP")
	flag.Parse()
	if *probe {
		if err := runProbe(*server, *name, *rrType, *tcp); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	s := &suite{ctx: context.Background(), report: report{
		Suite: "internal-dns-kubernetes", Started: time.Now().UTC(), Images: map[string]string{},
		Limitations: []string{
			"Kind clusters share one host; this run does not qualify regional or host failure.",
			"Project APIs use independent Kubernetes servers; Milo discovery and Karmada are not exercised.",
			"IPv6 context routes are a network fixture; Galactic private service connectivity and Compute attachment are not exercised.",
		},
	}}
	err := s.run(*envPath)
	s.report.Completed = time.Now().UTC()
	s.report.Passed = err == nil
	if err != nil {
		s.report.Error = err.Error()
		s.report.Checks = append(s.report.Checks, check{Name: "qualification completed", Passed: false, Time: time.Now().UTC(), Error: err.Error()})
	}
	if writeErr := writeReport(*results, s.report); writeErr != nil {
		fmt.Fprintln(os.Stderr, writeErr)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("Passed %d Kubernetes qualification checks; evidence: %s\n", len(s.report.Checks), *results)
}

func runProbe(server, name, rrType string, tcp bool) error {
	t, ok := dns.StringToType[strings.ToUpper(rrType)]
	if !ok || server == "" || name == "" {
		return errors.New("probe requires a server, name, and valid record type")
	}
	message := new(dns.Msg)
	message.SetQuestion(dns.Fqdn(name), t)
	transport := "udp"
	if tcp {
		transport = "tcp"
	}
	response, _, err := (&dns.Client{Net: transport, Timeout: 3 * time.Second}).Exchange(message, server)
	if err != nil {
		return json.NewEncoder(os.Stdout).Encode(probeResult{Error: err.Error()})
	}
	result := probeResult{Rcode: response.Rcode, Answers: []string{}, Addresses: []string{}}
	for _, answer := range response.Answer {
		result.Answers = append(result.Answers, answer.String())
		if result.TTL == 0 || answer.Header().Ttl < result.TTL {
			result.TTL = answer.Header().Ttl
		}
		switch record := answer.(type) {
		case *dns.A:
			result.Addresses = append(result.Addresses, record.A.String())
		case *dns.AAAA:
			result.Addresses = append(result.Addresses, record.AAAA.String())
		}
	}
	sort.Strings(result.Addresses)
	return json.NewEncoder(os.Stdout).Encode(result)
}

func writeReport(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func newClient(path string) (client.Client, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, err
	}
	cfg.Timeout = 8 * time.Second
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, dnsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

func (s *suite) pass(name string, evidence any) {
	s.report.Checks = append(s.report.Checks, check{Name: name, Passed: true, Time: time.Now().UTC(), Evidence: evidence})
	fmt.Println("PASS", name)
}

func (s *suite) wait(label string, duration time.Duration, test func() (bool, error)) error {
	deadline := time.Now().Add(duration)
	var last error
	for time.Now().Before(deadline) {
		passed, err := test()
		if passed && err == nil {
			return nil
		}
		if err != nil {
			last = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s: %v", label, last)
}

func objectRef(object client.Object) dnsv1.DNSObjectReference {
	return dnsv1.DNSObjectReference{Name: object.GetName(), UID: object.GetUID(), Generation: object.GetGeneration()}
}

func condition(conditions []metav1.Condition, name string, generation int64) bool {
	c := apimeta.FindStatusCondition(conditions, name)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == generation
}

func (s *suite) run(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &s.env); err != nil {
		return err
	}
	if len(s.env.Projects) != 2 || len(s.env.ControllerDeployments) == 0 || s.env.BrokerStatefulSet == "" {
		return errors.New("qualification requires two project APIs, controllers, and a broker")
	}
	if s.env.ProbeBinary == "" {
		s.env.ProbeBinary = "/dns-qualify"
	}
	if len(s.env.RegionalMembers) != 2 {
		return errors.New("qualification requires two independent regional BIND members")
	}
	s.platform, err = newClient(s.env.PlatformKubeconfig)
	if err != nil {
		return err
	}
	for i := range s.env.Projects {
		p := &s.env.Projects[i]
		for _, pair := range []struct {
			path   string
			target *client.Client
		}{
			{p.AdminKubeconfig, &p.admin}, {p.ProductKubeconfig, &p.product},
			{p.IssuerKubeconfig, &p.issuer}, {p.IntegrationKubeconfig, &p.integration},
		} {
			*pair.target, err = newClient(pair.path)
			if err != nil {
				return fmt.Errorf("%s client: %w", p.Name, err)
			}
		}
		if net.ParseIP(p.ConsumerAddress) == nil || p.ProjectUID == "" || p.SourceClusterUID == "" || p.PublisherSubject == "" {
			return fmt.Errorf("%s has incomplete identity or resolver address", p.Name)
		}
	}
	before, err := s.workloads()
	if err != nil {
		return err
	}
	if err := s.captureImages(); err != nil {
		return err
	}
	for i := range s.env.Projects {
		p := &s.env.Projects[i]
		if err := s.permissions(p); err != nil {
			return err
		}
		if err := s.prepare(p); err != nil {
			return err
		}
	}
	a, b := &s.env.Projects[0], &s.env.Projects[1]
	if a.resolverContext.UID == b.resolverContext.UID || a.zone.UID == b.zone.UID || a.resolverContext.Status.ManagedNamespace.Suffix == b.resolverContext.Status.ManagedNamespace.Suffix {
		return errors.New("source projects lost their distinct context, zone, or managed namespace identity")
	}
	s.pass("independent projects retain distinct context, zone, and managed namespace identities", map[string]string{"a": string(a.resolverContext.UID), "b": string(b.resolverContext.UID)})
	if err := s.scenarios(a, b); err != nil {
		return err
	}
	after, err := s.workloads()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, after) {
		return fmt.Errorf("contexts changed the shared workload count: before=%v after=%v", before, after)
	}
	s.pass("adding contexts and zones preserves the fixed shared workload count", after)
	return nil
}

func (s *suite) permissions(p *project) error {
	grant := &dnsv1.DNSContributionGrant{ObjectMeta: metav1.ObjectMeta{Name: "unauthorized", Namespace: p.Namespace}}
	if err := p.product.Create(s.ctx, grant); !apierrors.IsForbidden(err) {
		return fmt.Errorf("%s product grant creation must be forbidden: %v", p.Name, err)
	}
	contextObject := &dnsv1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "unauthorized", Namespace: p.Namespace}, Spec: dnsv1.DNSResolverContextSpec{ConsumerID: "unauthorized"}}
	if err := p.product.Create(s.ctx, contextObject); !apierrors.IsForbidden(err) {
		return fmt.Errorf("%s product network authorization must be forbidden: %v", p.Name, err)
	}
	access := &dnsv1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "unauthorized", Namespace: p.Namespace}}
	if err := p.product.Create(s.ctx, access); !apierrors.IsForbidden(err) {
		return fmt.Errorf("%s product access binding creation must be forbidden: %v", p.Name, err)
	}
	s.pass(p.Name+" product credentials cannot issue grants or authorize resolver access", nil)
	return nil
}

func (s *suite) prepare(p *project) error {
	p.resolverContext = dnsv1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "application-network", Namespace: p.Namespace}, Spec: dnsv1.DNSResolverContextSpec{ConsumerID: "qualification/" + p.Name, ManagedNamespace: dnsv1.DNSResolverContextManagedNamespace{Enabled: true}}}
	if err := p.integration.Create(s.ctx, &p.resolverContext); err != nil {
		return err
	}
	if err := s.wait(p.Name+" managed namespace", 120*time.Second, func() (bool, error) {
		err := p.product.Get(s.ctx, client.ObjectKeyFromObject(&p.resolverContext), &p.resolverContext)
		return condition(p.resolverContext.Status.Conditions, "Ready", p.resolverContext.Generation) && p.resolverContext.Status.ManagedNamespace.DNSZoneRef.UID != "", err
	}); err != nil {
		return err
	}
	s.pass(p.Name+" product discovers its managed namespace through the context API", p.resolverContext.Status.ManagedNamespace)
	p.zone = dnsv1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: p.Namespace}, Spec: dnsv1.DNSZoneSpec{DomainName: "prod.internal", DNSZoneClassName: "private-bind", Visibility: dnsv1.DNSZoneVisibilityPrivate}}
	if err := p.admin.Create(s.ctx, &p.zone); err != nil {
		return err
	}
	if err := s.associate(p, &p.zone, "prod"); err != nil {
		return err
	}
	p.access = dnsv1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "application-network-access", Namespace: p.Namespace}, Spec: dnsv1.DNSResolverAccessBindingSpec{
		ContextRef: objectRef(&p.resolverContext), Region: s.env.Region,
		QueryIdentity: dnsv1.DNSResolverQueryIdentity{Type: dnsv1.DNSResolverQueryIdentityDestinationAddress, Value: p.ConsumerAddress}, Port: 53, Transports: []string{"UDP", "TCP"},
		Authorization: dnsv1.DNSResolverAccessAuthorization{WriterEpoch: p.resolverContext.Status.AccessWriterEpoch, Sequence: 1, ValidUntil: metav1.NewTime(time.Now().Add(5 * time.Minute).UTC())},
	}}
	if err := p.integration.Create(s.ctx, &p.access); err != nil {
		return err
	}
	if err := s.waitAccess(p); err != nil {
		return err
	}
	s.pass(p.Name+" authorized destination becomes ready through shared Kubernetes members", p.ConsumerAddress)
	return nil
}

func (s *suite) associate(p *project, zone *dnsv1.DNSZone, name string) error {
	association := &dnsv1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.Namespace}, Spec: dnsv1.DNSZoneAssociationSpec{DNSZoneRef: objectRef(zone), ResolverContextRef: objectRef(&p.resolverContext)}}
	if err := p.admin.Create(s.ctx, association); err != nil {
		return err
	}
	return s.wait("private zone association "+p.Name+"/"+name, 60*time.Second, func() (bool, error) {
		err := p.admin.Get(s.ctx, client.ObjectKeyFromObject(association), association)
		return condition(association.Status.Conditions, "Accepted", association.Generation), err
	})
}

func (s *suite) waitAccess(p *project) error {
	return s.wait(p.Name+" resolver access ready", 120*time.Second, func() (bool, error) {
		err := p.integration.Get(s.ctx, client.ObjectKeyFromObject(&p.access), &p.access)
		if err != nil {
			return false, err
		}
		if !condition(p.access.Status.Conditions, "Ready", p.access.Generation) {
			return false, fmt.Errorf("current access conditions: %v", p.access.Status.Conditions)
		}
		return true, nil
	})
}

func recordSets(name, address, ipv6 string) []dnsv1.DNSContributionRecordSet {
	sets := []dnsv1.DNSContributionRecordSet{{RecordType: dnsv1.RRTypeA, Records: []dnsv1.RecordEntry{{Name: name, A: &dnsv1.ARecordSpec{Content: address}}}}}
	if ipv6 != "" {
		sets = append(sets, dnsv1.DNSContributionRecordSet{RecordType: dnsv1.RRTypeAAAA, Records: []dnsv1.RecordEntry{{Name: name, AAAA: &dnsv1.AAAARecordSpec{Content: ipv6}}}})
	}
	return sets
}

func (s *suite) publish(p *project, zone dnsv1.DNSObjectReference, prefix, name, address, ipv6 string, lifetime time.Duration) (*publication, error) {
	pub := &publication{registration: dnsv1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: prefix + "-registration", Namespace: p.Namespace}, Spec: dnsv1.DNSRegistrationSpec{DNSZoneRef: zone, Name: name, RecordTypes: []dnsv1.RRType{dnsv1.RRTypeA}, PublicationPolicy: dnsv1.DNSPublicationPolicyEligibleContributions, TTLSeconds: 2}}}
	if ipv6 != "" {
		pub.registration.Spec.RecordTypes = append(pub.registration.Spec.RecordTypes, dnsv1.RRTypeAAAA)
	}
	if err := p.product.Create(s.ctx, &pub.registration); err != nil {
		return nil, err
	}
	pub.grant = dnsv1.DNSContributionGrant{ObjectMeta: metav1.ObjectMeta{Name: prefix + "-compute", Namespace: p.Namespace}, Spec: dnsv1.DNSContributionGrantSpec{RegistrationRef: objectRef(&pub.registration), ProducerID: "compute", Principal: dnsv1.DNSProducerPrincipal{ClusterUID: p.SourceClusterUID, Subject: p.PublisherSubject}, RecordTypes: pub.registration.Spec.RecordTypes}}
	if err := p.issuer.Create(s.ctx, &pub.grant); err != nil {
		return nil, err
	}
	if err := s.wait("grant writer epoch", 60*time.Second, func() (bool, error) {
		err := p.product.Get(s.ctx, client.ObjectKeyFromObject(&pub.grant), &pub.grant)
		return pub.grant.Status.ActiveWriterEpoch > 0, err
	}); err != nil {
		return nil, err
	}
	pub.contribution = dnsv1.DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: prefix + "-endpoint", Namespace: p.Namespace}, Spec: dnsv1.DNSRecordContributionSpec{RegistrationRef: objectRef(&pub.registration), GrantRef: objectRef(&pub.grant), RecordSets: recordSets(name, address, ipv6)}}
	if err := p.product.Create(s.ctx, &pub.contribution); err != nil {
		return nil, err
	}
	if err := s.wait("bound contribution epoch", 60*time.Second, func() (bool, error) {
		err := p.product.Get(s.ctx, client.ObjectKeyFromObject(&pub.contribution), &pub.contribution)
		return pub.contribution.Status.WriterEpoch == pub.grant.Status.ActiveWriterEpoch, err
	}); err != nil {
		return nil, err
	}
	if err := s.observe(p, pub, true, lifetime); err != nil {
		return nil, err
	}
	return pub, nil
}

func (s *suite) observe(p *project, pub *publication, eligible bool, lifetime time.Duration) error {
	if err := p.product.Get(s.ctx, client.ObjectKeyFromObject(&pub.contribution), &pub.contribution); err != nil {
		return err
	}
	before := pub.contribution.DeepCopy()
	pub.sequence++
	pub.contribution.Status.Sequence = pub.sequence
	pub.contribution.Status.ObservedGeneration = pub.contribution.Generation
	pub.contribution.Status.Eligible = eligible
	pub.contribution.Status.Reason = "EndpointReady"
	if !eligible {
		pub.contribution.Status.Reason = "EndpointNotReady"
	}
	deadline := metav1.NewTime(time.Now().Add(lifetime).UTC())
	pub.contribution.Status.ValidUntil = &deadline
	if err := p.product.Status().Patch(s.ctx, &pub.contribution, client.MergeFrom(before)); err != nil {
		return err
	}
	return s.waitPublication(pub)
}

func (s *suite) waitPublication(pub *publication) error {
	return s.wait("verified publication for "+string(pub.contribution.UID), 90*time.Second, func() (bool, error) {
		var publications dnsv1.DNSPublicationManifestList
		if err := s.platform.List(s.ctx, &publications, client.InNamespace(s.env.Namespace)); err != nil {
			return false, err
		}
		for _, manifest := range publications.Items {
			matched := false
			for _, fence := range manifest.Spec.ContributionFences {
				if fence.UID == pub.contribution.UID && fence.Sequence == pub.sequence {
					matched = true
				}
			}
			if !matched {
				continue
			}
			members := map[string]bool{}
			for _, ack := range manifest.Status.ReplicaAcknowledgements {
				if ack.Phase == "Verified" && ack.WriterEpoch == manifest.Spec.WriterEpoch && ack.Revision == manifest.Spec.Revision && ack.ValidUntil != nil && ack.ValidUntil.After(time.Now()) {
					members[ack.MemberID] = true
				}
			}
			if members[s.env.RegionalMembers[0]] && members[s.env.RegionalMembers[1]] {
				return true, nil
			}
		}
		return false, fmt.Errorf("no fresh proof from both regional members for contribution %s sequence %d", pub.contribution.UID, pub.sequence)
	})
}

func (s *suite) query(p *project, name, rrType string, tcp bool) (probeResult, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 12*time.Second)
	defer cancel()
	args := []string{"--kubeconfig", s.env.PlatformKubeconfig, "-n", s.env.Namespace, "exec", s.env.ProbePod}
	if s.env.ProbeContainer != "" {
		args = append(args, "-c", s.env.ProbeContainer)
	}
	args = append(args, "--", s.env.ProbeBinary, "--probe", "--server", net.JoinHostPort(p.ConsumerAddress, "53"), "--name", name, "--type", rrType)
	if tcp {
		args = append(args, "--tcp")
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return probeResult{}, fmt.Errorf("query %s: %w: %s", name, err, stderr.String())
	}
	var result probeResult
	if err := json.Unmarshal(output, &result); err != nil {
		return result, err
	}
	return result, nil
}

func (s *suite) expect(p *project, name, rrType string, tcp bool, rcode int, addresses ...string) error {
	sort.Strings(addresses)
	var result probeResult
	err := s.wait("DNS answer "+p.Name+" "+name, 60*time.Second, func() (bool, error) {
		var err error
		result, err = s.query(p, name, rrType, tcp)
		if err != nil {
			return false, err
		}
		if result.Error != "" {
			return false, fmt.Errorf("DNS transport: %s", result.Error)
		}
		if result.Rcode != rcode {
			return false, fmt.Errorf("rcode=%s answers=%v", dns.RcodeToString[result.Rcode], result.Answers)
		}
		if len(addresses) == 0 {
			if len(result.Answers) != 0 {
				return false, fmt.Errorf("unexpected answers=%v", result.Answers)
			}
			return true, nil
		}
		if !reflect.DeepEqual(result.Addresses, addresses) {
			return false, fmt.Errorf("addresses=%v want=%v", result.Addresses, addresses)
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	transport := "UDP"
	if tcp {
		transport = "TCP"
	}
	s.pass(p.Name+" "+transport+" "+rrType+" "+name, result)
	return nil
}

func (s *suite) workloads() (map[string]int32, error) {
	values := map[string]int32{}
	var deployments appsv1.DeploymentList
	if err := s.platform.List(s.ctx, &deployments, client.InNamespace(s.env.Namespace)); err != nil {
		return nil, err
	}
	for _, value := range deployments.Items {
		if value.Spec.Replicas != nil {
			values["deployment/"+value.Name] = *value.Spec.Replicas
		}
	}
	var sets appsv1.StatefulSetList
	if err := s.platform.List(s.ctx, &sets, client.InNamespace(s.env.Namespace)); err != nil {
		return nil, err
	}
	for _, value := range sets.Items {
		if value.Spec.Replicas != nil {
			values["statefulset/"+value.Name] = *value.Spec.Replicas
		}
	}
	var daemonSets appsv1.DaemonSetList
	if err := s.platform.List(s.ctx, &daemonSets, client.InNamespace(s.env.Namespace)); err != nil {
		return nil, err
	}
	for _, value := range daemonSets.Items {
		values["daemonset/"+value.Name] = value.Status.DesiredNumberScheduled
	}
	var pods corev1.PodList
	if err := s.platform.List(s.ctx, &pods, client.InNamespace(s.env.Namespace)); err != nil {
		return nil, err
	}
	for _, pod := range pods.Items {
		if len(pod.OwnerReferences) == 0 {
			values["pod/"+pod.Name] = 1
		}
	}
	return values, nil
}

func (s *suite) captureImages() error {
	var pods corev1.PodList
	if err := s.platform.List(s.ctx, &pods, client.InNamespace(s.env.Namespace)); err != nil {
		return err
	}
	for _, pod := range pods.Items {
		for _, c := range pod.Status.ContainerStatuses {
			s.report.Images[pod.Name+"/"+c.Name] = c.ImageID
		}
	}
	return nil
}
