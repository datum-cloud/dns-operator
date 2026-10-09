package controlplane

import (
	"context"
	"encoding/json"
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
	requireAccessTestSuccess(t, dnsv1alpha1.AddToScheme(scheme))
	requireAccessTestSuccess(t, corev1.AddToScheme(scheme))
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, ns := range []string{"project", "platform"} {
		requireAccessTestSuccess(t, cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	}
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
	deadline := metav1.NewTime(now.Add(time.Minute + 22357123*time.Nanosecond))
	access := &dnsv1alpha1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: "project"}, Spec: dnsv1alpha1.DNSResolverAccessBindingSpec{ContextRef: dnsv1alpha1.DNSObjectReference{Name: resolverContext.Name, UID: resolverContext.UID}, Region: "central", QueryIdentity: dnsv1alpha1.DNSResolverQueryIdentity{Type: dnsv1alpha1.DNSResolverQueryIdentityDestinationAddress, Value: "fd70:100::10"}, Port: 53, Transports: []string{"UDP", "TCP"}, Authorization: dnsv1alpha1.DNSResolverAccessAuthorization{WriterEpoch: 1, Sequence: 2, ValidUntil: deadline}}}
	access.APIVersion = dnsv1alpha1.GroupVersion.String()
	access.Kind = "DNSResolverAccessBinding"
	raw, err := json.Marshal(access)
	if err != nil {
		t.Fatal(err)
	}
	// Submit unstructured JSON, as kubectl does, so the API initially stores a
	// fractional deadline independently of the typed client encoder.
	rawAccess := &unstructured.Unstructured{}
	requireAccessTestSuccess(t, json.Unmarshal(raw, &rawAccess.Object))
	rawAccess.Object["spec"].(map[string]interface{})["authorization"].(map[string]interface{})["validUntil"] = deadline.UTC().Format(time.RFC3339Nano)
	requireAccessTestSuccess(t, cl.Create(ctx, rawAccess))
	requireAccessTestSuccess(t, cl.Get(ctx, client.ObjectKeyFromObject(access), access))
	r := &Reconciler{Client: cl, Scheme: scheme, Options: ReconcilerOptions{ProjectUID: "project-uid", PlatformNamespace: "platform", Region: "central", Shard: "shared", MaxAccessLease: 5 * time.Minute, AddressAllocator: &platform.Allocator{Client: cl, Namespace: "platform"}}}
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
	if !binding.Spec.Authorization.ValidUntil.Equal(&deadline) {
		t.Fatalf("binding projection changed fractional deadline: %v != %v", binding.Spec.Authorization.ValidUntil, deadline)
	}
	bindingKey := client.ObjectKeyFromObject(&binding)
	readBinding := func() {
		t.Helper()
		requireAccessTestSuccess(t, cl.Get(ctx, bindingKey, &binding))
	}
	reconcile(now)
	readBinding()
	if binding.Spec.Tombstone || !binding.Spec.Authorization.ValidUntil.Equal(&deadline) {
		t.Fatal("unchanged fractional authorization was withdrawn on second reconcile")
	}
	requireAccessTestSuccess(t, cl.Get(ctx, client.ObjectKeyFromObject(access), access))
	if accepted := apimeta.FindStatusCondition(access.Status.Conditions, "Accepted"); accepted == nil || accepted.Status != metav1.ConditionTrue {
		t.Fatal("fractional source authorization lost Accepted status")
	}
	subsecondMutation := binding.DeepCopy()
	subsecondMutation.Spec.Authorization.ValidUntil = metav1.NewTime(deadline.Add(time.Nanosecond))
	if err := cl.Update(ctx, subsecondMutation); !apierrors.IsInvalid(err) {
		t.Fatalf("same-fence nanosecond deadline mutation accepted: %v", err)
	}
	// False is omitted from the wire. An ordinary active update must still
	// evaluate the optional tombstone transition rules without a CEL error.
	active := binding.DeepCopy()
	active.Labels["validation"] = "active-update"
	if err := cl.Update(ctx, active); err != nil {
		t.Fatalf("active update with omitted false tombstone failed: %v", err)
	}
	readBinding()
	for _, change := range []struct {
		name   string
		mutate func(*dnsv1alpha1.DNSResolverBinding)
	}{
		{"context UID", func(b *dnsv1alpha1.DNSResolverBinding) { b.Spec.Source.ResolverContextRef.UID = "replacement-context" }},
		{"access UID", func(b *dnsv1alpha1.DNSResolverBinding) { b.Spec.Source.AccessBindingRef.UID = "replacement-access" }},
		{"placement", func(b *dnsv1alpha1.DNSResolverBinding) { b.Spec.Placement.Shard = "replacement-shard" }},
		{"listener", func(b *dnsv1alpha1.DNSResolverBinding) { b.Spec.Configuration.Listeners.Node.Address = "fd70:100::11" }},
	} {
		mutated := binding.DeepCopy()
		change.mutate(mutated)
		mutated.Spec.Configuration.Revision++
		if err := cl.Update(ctx, mutated); !apierrors.IsInvalid(err) {
			t.Fatalf("immutable %s change accepted: %v", change.name, err)
		}
	}
	// Expiry advances the DNS-owned configuration fence, preserving the exact
	// integration-owned authorization. Source sequence 3 remains available.
	expiredAt := now.Add(61 * time.Second)
	reconcile(expiredAt)
	readBinding()
	if !binding.Spec.Tombstone || binding.Spec.Configuration.Revision != 2 || binding.Spec.Authorization.Sequence != 2 || !binding.Spec.Authorization.ValidUntil.Equal(&deadline) {
		t.Fatalf("withdrawal consumed source authority: %#v", binding.Spec)
	}
	requireAccessTestSuccess(t, cl.Get(ctx, client.ObjectKeyFromObject(access), access))
	staleExpiredAccess := *access.DeepCopy()
	// A direct same-fence replay cannot reopen the retired configuration.
	replay := binding.DeepCopy()
	replay.Spec.Tombstone = false
	replay.Spec.Configuration.Revision++
	if err := cl.Update(ctx, replay); !apierrors.IsInvalid(err) {
		t.Fatalf("same-fence reactivation accepted: %v", err)
	}
	mutated := binding.DeepCopy()
	mutated.Spec.Authorization.ValidUntil = metav1.NewTime(expiredAt.Add(time.Minute + 987654321*time.Nanosecond))
	if err := cl.Update(ctx, mutated); !apierrors.IsInvalid(err) {
		t.Fatalf("same-fence deadline mutation accepted: %v", err)
	}
	// Even if the source API has no admission webhook, the controller compares
	// against its retained authorization fence and rejects a deadline replay.
	requireAccessTestSuccess(t, cl.Get(ctx, client.ObjectKeyFromObject(access), access))
	baseAccess := access.DeepCopy()
	renewedDeadline := metav1.NewTime(expiredAt.Add(time.Minute + 987654321*time.Nanosecond))
	access.Spec.Authorization.ValidUntil = renewedDeadline
	requireAccessTestSuccess(t, cl.Patch(ctx, access, client.MergeFromWithOptions(baseAccess, client.MergeFromWithOptimisticLock{})))
	requireAccessTestSuccess(t, cl.Get(ctx, client.ObjectKeyFromObject(access), access))
	if !access.Spec.Authorization.ValidUntil.Equal(&renewedDeadline) {
		t.Fatal("typed merge patch lost source deadline precision")
	}
	reconcile(expiredAt)
	readBinding()
	if !binding.Spec.Tombstone {
		t.Fatal("same source fence reopened withdrawn access")
	}
	// Ordinary next-sequence renewal is independent of private DNS counters.
	requireAccessTestSuccess(t, cl.Get(ctx, client.ObjectKeyFromObject(access), access))
	access.Spec.Authorization.Sequence = 3
	requireAccessTestSuccess(t, cl.Update(ctx, access))
	reconcile(expiredAt)
	readBinding()
	if binding.Spec.Tombstone || binding.Spec.Authorization.Sequence != 3 || binding.Spec.Configuration.Revision != 3 || !binding.Spec.Authorization.ValidUntil.Equal(&access.Spec.Authorization.ValidUntil) {
		t.Fatalf("next source sequence did not recover access: %#v", binding.Spec)
	}
	// Another controller can List expired source sequence 2 before this renewed
	// platform binding is visible. Cleanup must freshly read sequence 3 instead
	// of converting an unchanged stale rejection status into revocation.
	staleController := &Reconciler{Client: &accessListSnapshotClient{Client: cl, accesses: []dnsv1alpha1.DNSResolverAccessBinding{staleExpiredAccess}}, Scheme: scheme, Options: r.Options}
	requireAccessTestSuccess(t, staleController.reconcileContextBindings(ctx, "project", nil, map[string]*dnsv1alpha1.DNSZone{}, expiredAt))
	readBinding()
	if binding.Spec.Tombstone || binding.Spec.Authorization.Sequence != 3 || binding.Spec.Configuration.Revision != 3 {
		t.Fatalf("stale source List withdrew renewed binding: %#v", binding.Spec)
	}
	staleController.Client = &accessListSnapshotClient{Client: cl}
	requireAccessTestSuccess(t, staleController.reconcileContextBindings(ctx, "project", nil, map[string]*dnsv1alpha1.DNSZone{}, expiredAt))
	readBinding()
	if binding.Spec.Tombstone || binding.Spec.Configuration.Revision != 3 {
		t.Fatal("omitted source List withdrew renewed binding")
	}
	// Deleting the context revokes access before its source deadline. Recreating
	// the same name cannot satisfy the old UID-pinned integration reference.
	oldContextUID := resolverContext.UID
	requireAccessTestSuccess(t, cl.Delete(ctx, resolverContext))
	reconcile(expiredAt)
	readBinding()
	if !binding.Spec.Tombstone || binding.Spec.Authorization.Sequence != 3 || !binding.Spec.Authorization.ValidUntil.Equal(&access.Spec.Authorization.ValidUntil) {
		t.Fatalf("context withdrawal rewrote source authority: %#v", binding.Spec)
	}
	freshContext := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "context", Namespace: "project"}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "opaque-consumer"}}
	readyContext(freshContext)
	if freshContext.UID == oldContextUID {
		t.Fatal("context UID did not rotate")
	}
	reconcile(expiredAt)
	readBinding()
	if !binding.Spec.Tombstone {
		t.Fatal("recreated context name restored old access")
	}
	// A new access UID cannot reuse a quarantined destination. A fresh protected
	// destination creates a separate binding and leaves the retired one closed.
	requireAccessTestSuccess(t, cl.Delete(ctx, access))
	freshAccess := &dnsv1alpha1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: "project"}, Spec: access.Spec}
	freshAccess.Spec.ContextRef = dnsv1alpha1.DNSObjectReference{Name: freshContext.Name, UID: freshContext.UID}
	freshAccess.Spec.Authorization.Sequence = 1
	requireAccessTestSuccess(t, cl.Create(ctx, freshAccess))
	requireAccessTestSuccess(t, cl.Get(ctx, client.ObjectKeyFromObject(freshAccess), freshAccess))
	if !freshAccess.Spec.Authorization.ValidUntil.Equal(&renewedDeadline) {
		t.Fatal("typed create lost source deadline precision")
	}
	reconcile(expiredAt)
	requireAccessTestSuccess(t, cl.List(ctx, &bindings, client.InNamespace("platform")))
	if len(bindings.Items) != 1 {
		t.Fatal("new access lifetime reused a protected destination")
	}
	requireAccessTestSuccess(t, cl.Get(ctx, client.ObjectKeyFromObject(freshAccess), freshAccess))
	freshAccess.Spec.QueryIdentity.Value = "fd70:100::11"
	freshAccess.Spec.Authorization.Sequence = 2
	requireAccessTestSuccess(t, cl.Update(ctx, freshAccess))
	reconcile(expiredAt)
	requireAccessTestSuccess(t, cl.List(ctx, &bindings, client.InNamespace("platform")))
	if len(bindings.Items) != 2 {
		t.Fatalf("fresh protected identity created %d bindings", len(bindings.Items))
	}
	readBinding()
	if !binding.Spec.Tombstone {
		t.Fatal("old binding reopened after access UID recreation")
	}
	// A renewal committed after the source confirmation but before destructive
	// platform Patch must win through the platform resourceVersion fence.
	var freshBinding dnsv1alpha1.DNSResolverBinding
	for _, candidate := range bindings.Items {
		if candidate.Spec.Source.AccessBindingRef.UID == freshAccess.UID {
			freshBinding = candidate
		}
	}
	if freshBinding.UID == "" {
		t.Fatal("fresh access binding missing")
	}
	requireAccessTestSuccess(t, cl.Get(ctx, client.ObjectKeyFromObject(freshAccess), freshAccess))
	freshAccess.Spec.Authorization.Sequence = 3
	freshAccess.Spec.Authorization.ValidUntil = metav1.NewTime(expiredAt.Add(-time.Second))
	requireAccessTestSuccess(t, cl.Update(ctx, freshAccess))
	raceClient := &bindingPatchRaceClient{Client: cl, beforeWithdrawal: func(ctx context.Context) error {
		if err := cl.Get(ctx, client.ObjectKeyFromObject(freshAccess), freshAccess); err != nil {
			return err
		}
		freshAccess.Spec.Authorization.Sequence = 4
		freshAccess.Spec.Authorization.ValidUntil = renewedDeadline
		if err := cl.Update(ctx, freshAccess); err != nil {
			return err
		}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(&freshBinding), &freshBinding); err != nil {
			return err
		}
		freshBinding.Spec.Authorization = freshAccess.Spec.Authorization
		freshBinding.Spec.Configuration.Revision++
		return cl.Update(ctx, &freshBinding)
	}}
	raceController := &Reconciler{Client: &accessListSnapshotClient{Client: raceClient}, Scheme: scheme, Options: r.Options}
	if err := raceController.reconcileContextBindings(ctx, "project", nil, nil, expiredAt); !apierrors.IsConflict(err) {
		t.Fatalf("concurrent platform renewal did not fence withdrawal: %v", err)
	}
	requireAccessTestSuccess(t, cl.Get(ctx, client.ObjectKeyFromObject(&freshBinding), &freshBinding))
	if freshBinding.Spec.Tombstone || freshBinding.Spec.Authorization.Sequence != 4 || !freshBinding.Spec.Authorization.ValidUntil.Equal(&renewedDeadline) {
		t.Fatal("destructive cleanup overwrote concurrent platform renewal")
	}
}

func requireAccessTestSuccess(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
