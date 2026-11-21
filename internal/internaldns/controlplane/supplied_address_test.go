// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns/api/v1alpha1"
	"go.miloapis.com/dns/internal/internaldns/platform"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type suppliedFixture struct {
	t      *testing.T
	client client.Client
	r      *Reconciler
	access *dnsv1alpha1.DNSResolverAccessBinding
	assoc  *dnsv1alpha1.DNSZoneAssociation
	zone   *dnsv1alpha1.DNSZone
	now    time.Time
}

func newSuppliedFixture(t *testing.T, regional string, require bool, objects ...client.Object) *suppliedFixture {
	t.Helper()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	contextObject := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "ctx", Namespace: "project", UID: "context-uid", Generation: 1}}
	setCondition(&contextObject.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "", 1, now)
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: "project", UID: "zone-uid", Generation: 1}, Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	assoc := &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: "assoc", Namespace: "project", UID: "assoc-uid", Generation: 1}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: "zone"}, ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: "ctx"}}, Status: dnsv1alpha1.DNSZoneAssociationStatus{ResolvedDNSZoneRef: dnsv1alpha1.DNSNameReference{Name: "zone"}, ResolvedResolverContextRef: dnsv1alpha1.DNSNameReference{Name: "ctx"}}}
	setCondition(&assoc.Status.Conditions, "Accepted", metav1.ConditionTrue, "Accepted", "", 1, now)
	access := &dnsv1alpha1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: "project", UID: "access-uid", Generation: 1}, Spec: dnsv1alpha1.DNSResolverAccessBindingSpec{ContextRef: dnsv1alpha1.DNSObjectReference{Name: "ctx", UID: "context-uid"}, Region: "central", DestinationAddress: "fd70:100::10", RegionalAddress: regional}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append([]client.Object{contextObject, zone, assoc, access}, objects...)...).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSResolverBinding{}).Build()
	allocator := &platform.Allocator{Client: c, Namespace: "platform", ClusterPrefix: "fd54::/64"}
	r := &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{ProjectUID: "project-uid", PlatformNamespace: "platform", Region: "central", Shard: "shared", AddressAllocator: allocator, RequireSuppliedAddresses: require}}
	seedReferenceState(t, r, assoc, map[string]dnsv1alpha1.DNSObjectReference{"zone": {Name: zone.Name, UID: zone.UID, Generation: zone.Generation}, "context": {Name: contextObject.Name, UID: contextObject.UID}})
	return &suppliedFixture{t: t, client: c, r: r, access: access, assoc: assoc, zone: zone, now: now}
}

func (f *suppliedFixture) reconcile() (*dnsv1alpha1.DNSResolverAccessBinding, []dnsv1alpha1.DNSResolverBinding) {
	f.t.Helper()
	ctx := context.Background()
	if err := f.r.reconcileContextBindings(ctx, "project", []dnsv1alpha1.DNSZoneAssociation{*f.assoc}, map[string]*dnsv1alpha1.DNSZone{"zone": f.zone}, f.now); err != nil {
		f.t.Fatal(err)
	}
	var access dnsv1alpha1.DNSResolverAccessBinding
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.access), &access); err != nil {
		f.t.Fatal(err)
	}
	var bindings dnsv1alpha1.DNSResolverBindingList
	if err := f.client.List(ctx, &bindings, client.InNamespace("platform"), client.MatchingLabels{"internal-dns.miloapis.com/access-uid": "access-uid"}); err != nil {
		f.t.Fatal(err)
	}
	return &access, bindings.Items
}

func (f *suppliedFixture) configMaps() int {
	f.t.Helper()
	var maps corev1.ConfigMapList
	if err := f.client.List(context.Background(), &maps, client.InNamespace("platform")); err != nil {
		f.t.Fatal(err)
	}
	n := 0
	for _, cm := range maps.Items {
		if cm.Labels["internal-dns.miloapis.com/address-lease"] == "true" {
			f.t.Logf("allocator ConfigMap %s", cm.Name)
			n++
		}
	}
	return n
}

func accepted(access *dnsv1alpha1.DNSResolverAccessBinding) *metav1.Condition {
	return apimeta.FindStatusCondition(access.Status.Conditions, dnsValueAccepted)
}

func TestSuppliedRegionalAddressIsServedAndReported(t *testing.T) {
	f := newSuppliedFixture(t, "fd54::99", false)
	access, bindings := f.reconcile()
	if len(bindings) != 1 || bindings[0].Spec.Configuration.Query.RegionalAddress != "fd54::99" {
		t.Fatalf("supplied address was not used: %#v", bindings)
	}
	if access.Status.RegionalAddress != "fd54::99" {
		t.Fatalf("status did not report the address: %#v", access.Status)
	}
	// Without the switch the address is still claimed, so the allocator
	// cannot hand it to another context.
	if err := (&platform.Allocator{Client: f.client, Namespace: "platform", ClusterPrefix: "fd54::/64"}).ClaimRegional(context.Background(), "other-project", "other-context", "central", "fd54::99"); err == nil {
		t.Fatal("a supplied address was not reserved")
	}

	revision := bindings[0].Spec.Configuration.Revision
	access.Spec.RegionalAddress = "fd54::98"
	if err := f.client.Update(context.Background(), access); err != nil {
		t.Fatal(err)
	}
	access, bindings = f.reconcile()
	if bindings[0].Spec.Configuration.Query.RegionalAddress != "fd54::98" || bindings[0].Spec.Configuration.Revision != revision+1 || access.Status.RegionalAddress != "fd54::98" {
		t.Fatalf("address change was not applied with a new revision: %#v", bindings[0].Spec.Configuration)
	}
}

func TestAllocatedRegionalAddressIsReported(t *testing.T) {
	f := newSuppliedFixture(t, "", false)
	access, bindings := f.reconcile()
	if len(bindings) != 1 || bindings[0].Spec.Configuration.Query.RegionalAddress == "" || access.Status.RegionalAddress != bindings[0].Spec.Configuration.Query.RegionalAddress {
		t.Fatalf("allocated address was not reported: %#v", access.Status)
	}
}

func TestSuppliedRegionalAddressOutsideRangeIsRefused(t *testing.T) {
	f := newSuppliedFixture(t, "fd99::1", false)
	access, bindings := f.reconcile()
	if len(bindings) != 0 || accepted(access) == nil || accepted(access).Reason != "RegionalAddressRefused" {
		t.Fatalf("out-of-range address was accepted: %#v", access.Status.Conditions)
	}
}

func TestRequiredSuppliedAddressesMakeNoClaims(t *testing.T) {
	f := newSuppliedFixture(t, "", true)
	access, bindings := f.reconcile()
	if len(bindings) != 0 || accepted(access).Reason != "RegionalAddressRequired" {
		t.Fatalf("missing address was accepted: %#v", access.Status.Conditions)
	}
	access.Spec.RegionalAddress = "fd54::99"
	if err := f.client.Update(context.Background(), access); err != nil {
		t.Fatal(err)
	}
	access, bindings = f.reconcile()
	if len(bindings) != 1 || access.Status.RegionalAddress != "fd54::99" || accepted(access).Status != metav1.ConditionTrue {
		t.Fatalf("supplied address was not served: %#v", access.Status)
	}
	if n := f.configMaps(); n != 0 {
		t.Fatalf("created %d allocator ConfigMaps", n)
	}
}

func TestRequiredSuppliedAddressInUseIsRefused(t *testing.T) {
	other := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "platform", Labels: map[string]string{}}, Spec: dnsv1alpha1.DNSResolverBindingSpec{
		Source:        dnsv1alpha1.DNSResolverBindingSource{ProjectUID: "other-project", AccessBindingRef: dnsv1alpha1.DNSObjectReference{Name: "access", UID: "other-access"}},
		Placement:     dnsv1alpha1.DNSResolverBindingPlacement{Region: "central", Shard: "shared"},
		Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Generation: 1, Revision: 1, Query: dnsv1alpha1.DNSResolverBindingQuery{NodeAddress: "fd70:100::77", RegionalAddress: "fd54::99"}},
	}}
	addressLabels(other.Labels, "central", "fd70:100::77", "fd54::99")
	f := newSuppliedFixture(t, "fd54::99", true, other)
	access, bindings := f.reconcile()
	if len(bindings) != 0 || accepted(access).Reason != "AddressInUse" {
		t.Fatalf("an address another binding serves was accepted: %#v", access.Status.Conditions)
	}

	access.Spec.RegionalAddress = "fd54::98"
	if err := f.client.Update(context.Background(), access); err != nil {
		t.Fatal(err)
	}
	if _, bindings = f.reconcile(); len(bindings) != 1 {
		t.Fatal("a free address was refused")
	}
	// Moving an existing binding onto a taken address keeps the old one.
	access, _ = f.reconcile()
	access.Spec.RegionalAddress = "fd54::99"
	if err := f.client.Update(context.Background(), access); err != nil {
		t.Fatal(err)
	}
	access, bindings = f.reconcile()
	applied := apimeta.FindStatusCondition(access.Status.Conditions, "AddressesApplied")
	if len(bindings) != 1 || bindings[0].Spec.Configuration.Query.RegionalAddress != "fd54::98" || access.Status.RegionalAddress != "fd54::98" || applied == nil || applied.Reason != "AddressInUse" {
		t.Fatalf("a taken address replaced a served one: %#v %#v", bindings, access.Status)
	}
}
