package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/platform"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestAccessRecoveryPreservesSourceFencesWithCEL exercises the actual CRD
// transition rules together with controller withdrawal, renewal, and UID pins.
func TestAccessRecoveryPreservesSourceFencesWithCEL(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run access recovery against the API server")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	requireAccessTestSuccess(t, addReferenceStateToScheme(scheme))
	requireAccessTestSuccess(t, corev1.AddToScheme(scheme))
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, ns := range []string{"project", "platform"} {
		requireAccessTestSuccess(t, cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	}
	t.Run("requested managed domain admission", func(t *testing.T) {
		for name, domain := range map[string]string{"missing": "", "invalid": "bad..internal", "wildcard": "*.internal"} {
			resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "project"}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: name, ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespace{Enabled: true, DomainSuffix: domain}}}
			if err := cl.Create(ctx, resolver); !apierrors.IsInvalid(err) {
				t.Fatalf("invalid managed domain %q was not rejected: %v", domain, err)
			}
		}
		resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "custom-domain", Namespace: "project"}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "custom", ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespace{Enabled: true, DomainSuffix: "private.example.net"}}}
		requireAccessTestSuccess(t, cl.Create(ctx, resolver))
		changed := resolver.DeepCopy()
		changed.Spec.ManagedNamespace.DomainSuffix = "different.internal"
		if err := cl.Update(ctx, changed); !apierrors.IsInvalid(err) {
			t.Fatalf("domain mutation was not rejected: %v", err)
		}
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(resolver)
		requireAccessTestSuccess(t, err)
		unstructured.RemoveNestedField(raw, "spec", "managedNamespace")
		removed := &unstructured.Unstructured{Object: raw}
		removed.SetAPIVersion(dnsv1alpha1.GroupVersion.String())
		removed.SetKind("DNSResolverContext")
		if err := cl.Update(ctx, removed); !apierrors.IsInvalid(err) {
			t.Fatalf("domain removal was not rejected: %v", err)
		}
	})
	now := time.Now().UTC().Truncate(time.Second)
	resolverContext := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "context", Namespace: "project"}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "opaque-consumer"}}
	readyContext := func(c *dnsv1alpha1.DNSResolverContext) {
		t.Helper()
		requireAccessTestSuccess(t, cl.Create(ctx, c))
		c.Status.AccessWriterEpoch = 1
		apimeta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: c.Generation, Reason: "Accepted", LastTransitionTime: metav1.NewTime(now)})
		requireAccessTestSuccess(t, cl.Status().Update(ctx, c))
	}
	readyContext(resolverContext)
	t.Run("name-only customer references", func(t *testing.T) {
		verifyNamedReferencesWithAPI(t, cl)
	})
	access := &dnsv1alpha1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: "project"}, Spec: dnsv1alpha1.DNSResolverAccessBindingSpec{ContextRef: dnsv1alpha1.DNSObjectReference{Name: resolverContext.Name, UID: resolverContext.UID}, Region: "central", QueryIdentity: dnsv1alpha1.DNSResolverQueryIdentity{Type: dnsv1alpha1.DNSResolverQueryIdentityDestinationAddress, Value: "fd70:100::10"}, Port: 53, Transports: []string{"UDP", "TCP"}, Authorization: dnsv1alpha1.DNSResolverAccessAuthorization{WriterEpoch: 1, Sequence: 2}}}
	requireAccessTestSuccess(t, cl.Create(ctx, access))
	r := &Reconciler{Client: cl, Scheme: scheme, Options: ReconcilerOptions{ProjectUID: "project-uid", PlatformNamespace: "platform", Region: "central", Shard: "shared", AddressAllocator: &platform.Allocator{Client: cl, Namespace: "platform"}}}
	reconcile := func(at time.Time) {
		t.Helper()
		requireAccessTestSuccess(t, r.reconcileContextBindings(ctx, "project", nil, map[string]*dnsv1alpha1.DNSZone{}, at))
	}
	reconcile(now)
	var bindings dnsv1alpha1.DNSResolverBindingList
	requireAccessTestSuccess(t, cl.List(ctx, &bindings, client.InNamespace("platform")))
	if len(bindings.Items) != 1 {
		t.Fatalf("got %d bindings", len(bindings.Items))
	}
	binding := bindings.Items[0]
	key := client.ObjectKeyFromObject(&binding)
	reconcile(now.Add(365 * 24 * time.Hour))
	requireAccessTestSuccess(t, cl.Get(ctx, key, &binding))
	if binding.Spec.Tombstone || binding.Spec.Authorization.Sequence != 2 || binding.Spec.Configuration.Revision != 1 {
		t.Fatalf("elapsed time changed access: %#v", binding.Spec)
	}
	requireAccessTestSuccess(t, cl.Get(ctx, client.ObjectKeyFromObject(access), access))
	// Schema validation preserves listener identity and ordered source revisions.
	for _, mutate := range []func(*dnsv1alpha1.DNSResolverAccessBinding){func(a *dnsv1alpha1.DNSResolverAccessBinding) { a.Spec.QueryIdentity.Value = "fd70:100::20" }, func(a *dnsv1alpha1.DNSResolverAccessBinding) { a.Spec.Authorization.Sequence = 1 }, func(a *dnsv1alpha1.DNSResolverAccessBinding) { a.Spec.ContextRef.UID = "another-context" }} {
		candidate := access.DeepCopy()
		mutate(candidate)
		if err := cl.Update(ctx, candidate); !apierrors.IsInvalid(err) {
			t.Fatalf("identity or revision mutation accepted: %v", err)
		}
	}
	// An incomplete cached List cannot revoke a live access binding.
	stale := &Reconciler{Client: &accessListSnapshotClient{Client: cl, accesses: nil}, Scheme: scheme, Options: r.Options}
	requireAccessTestSuccess(t, stale.reconcileContextBindings(ctx, "project", nil, nil, now.Add(365*24*time.Hour)))
	requireAccessTestSuccess(t, cl.Get(ctx, key, &binding))
	if binding.Spec.Tombstone {
		t.Fatal("incomplete List withdrew live access")
	}
	requireAccessTestSuccess(t, cl.Delete(ctx, resolverContext))
	reconcile(now.Add(365 * 24 * time.Hour))
	requireAccessTestSuccess(t, cl.Get(ctx, key, &binding))
	if !binding.Spec.Tombstone || binding.Spec.Authorization.Sequence != 2 {
		t.Fatal("explicit context deletion did not withdraw access")
	}
	replacement := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: resolverContext.Name, Namespace: "project"}, Spec: resolverContext.Spec}
	readyContext(replacement)
	reconcile(now.Add(365 * 24 * time.Hour))
	requireAccessTestSuccess(t, cl.Get(ctx, key, &binding))
	if !binding.Spec.Tombstone {
		t.Fatal("replacement context inherited retired access")
	}
	requireAccessTestSuccess(t, cl.Delete(ctx, access))
	access = &dnsv1alpha1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: "project"}, Spec: access.Spec}
	access.Spec.ContextRef.UID = replacement.UID
	access.Spec.QueryIdentity.Value = "fd70:100::20"
	access.Spec.Authorization.Sequence = 1
	requireAccessTestSuccess(t, cl.Create(ctx, access))
	reconcile(now.Add(365 * 24 * time.Hour))
	requireAccessTestSuccess(t, cl.List(ctx, &bindings, client.InNamespace("platform")))
	active := 0
	for _, b := range bindings.Items {
		if !b.Spec.Tombstone {
			active++
			if b.Spec.Source.ResolverContextRef.UID != replacement.UID {
				t.Fatal("new binding uses retired context")
			}
		}
	}
	if active != 1 {
		t.Fatalf("active bindings=%d", active)
	}
}

func requireAccessTestSuccess(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
