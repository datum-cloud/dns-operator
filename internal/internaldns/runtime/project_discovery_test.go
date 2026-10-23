package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"
	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	kubeadmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type discoveryFixtureClient struct {
	client.Client
	unavailable bool
}

func (c *discoveryFixtureClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if c.unavailable {
		return errors.New("discovery unavailable")
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *discoveryFixtureClient) List(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) error {
	if c.unavailable {
		return errors.New("discovery unavailable")
	}
	return c.Client.List(ctx, obj, opts...)
}

func TestProjectDiscoveryAddsSourcesAndPreservesThemDuringOutage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	first := readyDiscoveryProject("first", "first-uid")
	core := &discoveryFixtureClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(first).Build()}
	directory := newProjectDirectory(nil)
	starts := make(chan types.UID, 4)
	d := &projectDiscovery{client: core, directory: directory, rest: &rest.Config{Host: "https://milo.example:6443"}, makeProject: func(config ProjectConfig, sourceREST *rest.Config) (projectRuntime, error) {
		if config.Namespace != "" || !strings.HasSuffix(sourceREST.Host, "/projects/"+config.Name+"/control-plane") {
			t.Errorf("unexpected discovery scope: %#v %s", config, sourceREST.Host)
		}
		direct := fake.NewClientBuilder().WithScheme(scheme).Build()
		return projectRuntime{config: config, direct: direct, start: func(workerCtx context.Context) error {
			starts <- config.ProjectUID
			<-workerCtx.Done()
			return workerCtx.Err()
		}}, nil
	}}
	if err := d.step(ctx); err != nil {
		t.Fatal(err)
	}
	awaitDiscoveredSource(t, starts, first.UID)
	second := readyDiscoveryProject("second", "second-uid")
	if err := core.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := d.step(ctx); err != nil {
		t.Fatal(err)
	}
	awaitDiscoveredSource(t, starts, second.UID)
	scopes := directory.scopes()
	if len(scopes) != 2 || scopes[0].Namespace != "" || scopes[1].Namespace != "" {
		t.Fatalf("ACK scopes did not follow discovery: %#v", scopes)
	}
	if err := verifyProjectIdentity(ctx, core, first.Name, first.UID); err != nil {
		t.Fatal(err)
	}
	core.unavailable = true
	if err := verifyProjectIdentity(ctx, core, first.Name, first.UID); err == nil {
		t.Fatal("unobserved source identity permitted compilation")
	}
	if err := d.step(ctx); err == nil {
		t.Fatal("expected discovery error")
	}
	if len(directory.scopes()) != 2 {
		t.Fatal("discovery outage discarded existing sources")
	}
	core.unavailable = false
	if err := core.Get(ctx, client.ObjectKeyFromObject(first), first); err != nil {
		t.Fatal(err)
	}
	first.Status.Conditions[0].Status = metav1.ConditionFalse
	if err := core.Update(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := d.step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(directory.scopes()) != 2 {
		t.Fatal("unavailable project discarded its existing source")
	}
	request := kubeadmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Namespace: "internal-dns-platform", UserInfo: authenticationv1.UserInfo{Extra: map[string]authenticationv1.ExtraValue{parentNameExtra: {second.Name}}}}}
	selected, err := directory.selectProject(ctx, request)
	if err != nil || selected != scopes[1].Client {
		t.Fatalf("dynamic admission did not map all namespaces: %v", err)
	}
	if err := core.Delete(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := directory.selectProject(ctx, request); err == nil {
		t.Fatal("deleted project passed admission")
	}
	recreated := readyDiscoveryProject(second.Name, "replacement-uid")
	if err := core.Create(ctx, recreated); err != nil {
		t.Fatal(err)
	}
	if err := verifyProjectIdentity(ctx, core, recreated.Name, second.UID); err == nil {
		t.Fatal("recreated project used previous compilation identity")
	}
	if _, err := directory.selectProject(ctx, request); err == nil {
		t.Fatal("recreated project used previous source")
	}
	if err := d.step(ctx); err != nil {
		t.Fatal(err)
	}
	awaitDiscoveredSource(t, starts, recreated.UID)
	if _, err := directory.selectProject(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := core.Delete(ctx, recreated); err != nil {
		t.Fatal(err)
	}
	if err := d.step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(directory.scopes()) != 1 {
		t.Fatal("ACK scopes retained deleted source")
	}
}

func readyDiscoveryProject(name string, uid types.UID) *resourcemanagerv1alpha1.Project {
	return &resourcemanagerv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}, Status: resourcemanagerv1alpha1.ProjectStatus{Conditions: []metav1.Condition{{Type: resourcemanagerv1alpha1.ProjectReady, Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now()}}}}
}
func awaitDiscoveredSource(t *testing.T, starts <-chan types.UID, expected types.UID) {
	t.Helper()
	select {
	case actual := <-starts:
		if actual != expected {
			t.Fatalf("started %s want %s", actual, expected)
		}
	case <-time.After(time.Second):
		t.Fatal("new project source did not start")
	}
}

func TestAllNamespaceRoutingPreservesListsAndStatus(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	zone := &dnsv1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "same-name", Namespace: "internal-dns-platform"}}
	other := &dnsv1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: zone.Name, Namespace: "other"}}
	source := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone, other).WithStatusSubresource(zone, other).Build()
	managed := fake.NewClientBuilder().WithScheme(scheme).Build()
	routed := &RoutingClient{Client: managed, Source: source, PlatformNamespace: zone.Namespace}
	var zones dnsv1.DNSZoneList
	if err := routed.List(context.Background(), &zones); err != nil {
		t.Fatal(err)
	}
	if len(zones.Items) != 2 {
		t.Fatalf("source namespaces were dropped: %#v", zones.Items)
	}
	current := &zones.Items[0]
	base := current.DeepCopy()
	current.Status.Conditions = []metav1.Condition{{Type: "Published", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now()}}
	if err := routed.Status().Patch(context.Background(), current, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	if err := source.Get(context.Background(), client.ObjectKeyFromObject(current), current); err != nil || len(current.Status.Conditions) != 1 {
		t.Fatalf("status did not reach source: %v", err)
	}
}

func TestOneBrokenProjectDoesNotHoldDiscoveryReadiness(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	broken := readyDiscoveryProject("broken", "")
	core := &discoveryFixtureClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(broken).Build()}
	discovered := &firstSuccess{}
	d := &projectDiscovery{client: core, directory: newProjectDirectory(nil), rest: &rest.Config{Host: "https://milo.example:6443"}, passed: discovered.done,
		makeProject: func(ProjectConfig, *rest.Config) (projectRuntime, error) {
			return projectRuntime{}, errors.New("unexpected source")
		}}
	core.unavailable = true
	if err := d.step(context.Background()); err == nil {
		t.Fatal("expected discovery error")
	}
	if discovered.check(nil) == nil {
		t.Fatal("a failed project list made the control plane ready")
	}
	core.unavailable = false
	if err := d.step(context.Background()); err == nil {
		t.Fatal("a project with no identity passed discovery")
	}
	if err := discovered.check(nil); err != nil {
		t.Fatalf("one broken project held readiness: %v", err)
	}
}
