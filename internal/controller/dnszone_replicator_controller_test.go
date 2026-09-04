// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"reflect"
	"testing"
	"time"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type fakeStrategy struct {
	namespace string
	name      string
	client    client.Client
}

func (f fakeStrategy) GetClient() client.Client { return f.client }

func (f fakeStrategy) ObjectMetaFromUpstreamObject(context.Context, metav1.Object) (metav1.ObjectMeta, error) {
	return metav1.ObjectMeta{Namespace: f.namespace, Name: f.name}, nil
}

func (f fakeStrategy) SetControllerReference(context.Context, metav1.Object, metav1.Object, ...controllerutil.OwnerReferenceOption) error {
	return nil
}

func (f fakeStrategy) SetOwnerReference(context.Context, metav1.Object, metav1.Object, ...controllerutil.OwnerReferenceOption) error {
	return nil
}

func (f fakeStrategy) DeleteAnchorForObject(context.Context, client.Object) error {
	return nil
}

type countingStatusWriter struct {
	client.StatusWriter
	patchCount *int
}

func (w *countingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	*w.patchCount++
	return w.StatusWriter.Patch(ctx, obj, patch, opts...)
}

type countingClient struct {
	client.Client
	statusPatchCount int
}

func (c *countingClient) Status() client.StatusWriter {
	return &countingStatusWriter{
		StatusWriter: c.Client.Status(),
		patchCount:   &c.statusPatchCount,
	}
}

func TestDNSZoneReplicatorUpdateStatus_NoPatchForOrderOnlyChanges(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add dns scheme: %v", err)
	}
	if err := networkingv1alpha.AddToScheme(scheme); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "zone-a",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "pdns",
		},
		Status: dnsv1alpha1.DNSZoneStatus{
			Nameservers: []string{"ns2.example.com", "ns1.example.com"},
			RecordCount: 2,
			DomainRef: &dnsv1alpha1.DomainRef{
				Name: "example.com",
				Status: dnsv1alpha1.DomainRefStatus{
					Nameservers: []networkingv1alpha.Nameserver{
						{
							Hostname: "ns2.example.com",
							IPs: []networkingv1alpha.NameserverIP{
								{Address: "192.0.2.20"},
								{Address: "192.0.2.10"},
							},
						},
						{
							Hostname: "ns1.example.com",
							IPs: []networkingv1alpha.NameserverIP{
								{Address: "192.0.2.5"},
							},
						},
					},
				},
			},
		},
	}

	now := metav1.NewTime(time.Now())
	zone.Status.Conditions = []metav1.Condition{
		{
			Type:               CondAccepted,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonAccepted,
			Message:            "Nameservers retrieved from downstream",
			ObservedGeneration: zone.Generation,
			LastTransitionTime: now,
		},
		{
			Type:               CondProgrammed,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonProgrammed,
			Message:            "Default records ensured",
			ObservedGeneration: zone.Generation,
			LastTransitionTime: now,
		},
	}

	domain := &networkingv1alpha.Domain{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "example.com",
			Namespace: "default",
		},
		Spec: networkingv1alpha.DomainSpec{
			DomainName: "example.com",
		},
		Status: networkingv1alpha.DomainStatus{
			Nameservers: []networkingv1alpha.Nameserver{
				{
					Hostname: "ns1.example.com",
					IPs: []networkingv1alpha.NameserverIP{
						{Address: "192.0.2.5"},
					},
				},
				{
					Hostname: "ns2.example.com",
					IPs: []networkingv1alpha.NameserverIP{
						{Address: "192.0.2.10"},
						{Address: "192.0.2.20"},
					},
				},
			},
		},
	}

	soa := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "soa-records",
			Namespace: "default",
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: dnsv1alpha1.RRTypeSOA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "@", SOA: &dnsv1alpha1.SOARecordSpec{MName: "ns1.example.com"}},
			},
		},
	}
	ns := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ns-records",
			Namespace: "default",
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: dnsv1alpha1.RRTypeNS,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "@", NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.example.com"}},
			},
		},
	}

	downstreamZone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shadow-zone-a",
			Namespace: "downstream",
		},
		Status: dnsv1alpha1.DNSZoneStatus{
			Nameservers: []string{"ns1.example.com", "ns2.example.com"},
		},
	}

	upstreamBase := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSZone{}).
		WithObjects(zone, domain, soa, ns).
		WithIndex(&dnsv1alpha1.DNSRecordSet{}, "spec.dnsZoneRef.name", func(obj client.Object) []string {
			rs := obj.(*dnsv1alpha1.DNSRecordSet)
			if rs.Spec.DNSZoneRef.Name == "" {
				return nil
			}
			return []string{rs.Spec.DNSZoneRef.Name}
		}).
		WithIndex(&networkingv1alpha.Domain{}, "spec.domainName", func(obj client.Object) []string {
			d := obj.(*networkingv1alpha.Domain)
			if d.Spec.DomainName == "" {
				return nil
			}
			return []string{d.Spec.DomainName}
		}).
		Build()

	upstreamClient := &countingClient{Client: upstreamBase}
	downstreamClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(downstreamZone).Build()

	r := &DNSZoneReplicator{
		DownstreamClient: downstreamClient,
	}

	strategy := fakeStrategy{
		namespace: downstreamZone.Namespace,
		name:      downstreamZone.Name,
		client:    downstreamClient,
	}

	initialStatus := zone.Status.DeepCopy()
	if err := r.updateStatus(context.Background(), upstreamClient, strategy, zone); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}

	if upstreamClient.statusPatchCount != 0 {
		t.Fatalf("expected no status patch, got %d", upstreamClient.statusPatchCount)
	}
	if !statusEqualIgnoringTransitionTime(zone.Status, *initialStatus) {
		t.Fatalf("expected in-memory status unchanged (ignoring transition time)")
	}

	var stored dnsv1alpha1.DNSZone
	if err := upstreamClient.Get(context.Background(), client.ObjectKeyFromObject(zone), &stored); err != nil {
		t.Fatalf("get stored zone: %v", err)
	}
	if !statusEqualIgnoringTransitionTime(stored.Status, *initialStatus) {
		t.Fatalf("expected stored status unchanged (ignoring transition time)")
	}
	if apimeta.FindStatusCondition(stored.Status.Conditions, CondAccepted) == nil {
		t.Fatalf("expected accepted condition to remain present")
	}
}

func TestDNSZoneReplicatorIsDomainVerified(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := networkingv1alpha.AddToScheme(scheme); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}

	const ns = "default"

	verifiedDomain := &networkingv1alpha.Domain{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "verified.example.com"},
		Spec:       networkingv1alpha.DomainSpec{DomainName: "verified.example.com"},
		Status: networkingv1alpha.DomainStatus{
			Conditions: []metav1.Condition{
				{Type: networkingv1alpha.DomainConditionVerified, Status: metav1.ConditionTrue},
			},
		},
	}
	pendingDomain := &networkingv1alpha.Domain{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pending.example.com"},
		Spec:       networkingv1alpha.DomainSpec{DomainName: "pending.example.com"},
		Status: networkingv1alpha.DomainStatus{
			Conditions: []metav1.Condition{
				{Type: networkingv1alpha.DomainConditionVerified, Status: metav1.ConditionFalse},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(verifiedDomain, pendingDomain).
		WithIndex(&networkingv1alpha.Domain{}, "spec.domainName", func(obj client.Object) []string {
			d := obj.(*networkingv1alpha.Domain)
			if d.Spec.DomainName == "" {
				return nil
			}
			return []string{d.Spec.DomainName}
		}).
		Build()

	r := &DNSZoneReplicator{}

	t.Run("verified domain", func(t *testing.T) {
		t.Parallel()
		verified, err := r.isDomainVerified(context.Background(), fakeClient, ns, "verified.example.com")
		if err != nil {
			t.Fatalf("isDomainVerified: %v", err)
		}
		if !verified {
			t.Fatal("expected domain to be verified")
		}
	})

	t.Run("pending domain", func(t *testing.T) {
		t.Parallel()
		verified, err := r.isDomainVerified(context.Background(), fakeClient, ns, "pending.example.com")
		if err != nil {
			t.Fatalf("isDomainVerified: %v", err)
		}
		if verified {
			t.Fatal("expected domain to not be verified")
		}
	})

	t.Run("no matching domain", func(t *testing.T) {
		t.Parallel()
		verified, err := r.isDomainVerified(context.Background(), fakeClient, ns, "missing.example.com")
		if err != nil {
			t.Fatalf("isDomainVerified: %v", err)
		}
		if verified {
			t.Fatal("expected no domain to mean not verified")
		}
	})

	// A per-run subdomain of a domain the project already owns is the shape
	// that stopped provisioning when the gate landed: the project has proven
	// the parent, so the child needs no proof of its own.
	t.Run("subdomain of a verified domain", func(t *testing.T) {
		t.Parallel()
		verified, err := r.isDomainVerified(context.Background(), fakeClient, ns, "child.verified.example.com")
		if err != nil {
			t.Fatalf("isDomainVerified: %v", err)
		}
		if !verified {
			t.Fatal("expected a subdomain of a verified domain to be verified")
		}
	})

	t.Run("deep subdomain of a verified domain", func(t *testing.T) {
		t.Parallel()
		verified, err := r.isDomainVerified(context.Background(), fakeClient, ns, "a.b.verified.example.com")
		if err != nil {
			t.Fatalf("isDomainVerified: %v", err)
		}
		if !verified {
			t.Fatal("expected a deep subdomain of a verified domain to be verified")
		}
	})

	t.Run("subdomain of a pending domain", func(t *testing.T) {
		t.Parallel()
		verified, err := r.isDomainVerified(context.Background(), fakeClient, ns, "child.pending.example.com")
		if err != nil {
			t.Fatalf("isDomainVerified: %v", err)
		}
		if verified {
			t.Fatal("expected a subdomain of an unverified domain to stay unverified")
		}
	})

	t.Run("shared suffix is not a parent", func(t *testing.T) {
		t.Parallel()
		verified, err := r.isDomainVerified(context.Background(), fakeClient, ns, "notverified.example.com")
		if err != nil {
			t.Fatalf("isDomainVerified: %v", err)
		}
		if verified {
			t.Fatal("expected a name merely sharing a suffix to stay unverified")
		}
	})

	t.Run("verified domain in another namespace", func(t *testing.T) {
		t.Parallel()
		verified, err := r.isDomainVerified(context.Background(), fakeClient, "other", "child.verified.example.com")
		if err != nil {
			t.Fatalf("isDomainVerified: %v", err)
		}
		if verified {
			t.Fatal("expected another project's proof not to carry over")
		}
	})
}

// TestDNSZoneReplicatorEnsureDomainRefPublishesBeforeTheGate covers the link a
// zone needs while it is still held back: ownership verification finds zones
// through status.domainRef, so a zone waiting on verification with no link is
// invisible to the check that would release it.
func TestDNSZoneReplicatorEnsureDomainRefPublishesBeforeTheGate(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add dns scheme: %v", err)
	}
	if err := networkingv1alpha.AddToScheme(scheme); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pending-zone", Generation: 1},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "pending.example.com",
			DNSZoneClassName: "pdns",
		},
	}
	domain := &networkingv1alpha.Domain{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pending.example.com"},
		Spec:       networkingv1alpha.DomainSpec{DomainName: "pending.example.com"},
		Status: networkingv1alpha.DomainStatus{
			Conditions: []metav1.Condition{
				{Type: networkingv1alpha.DomainConditionVerified, Status: metav1.ConditionFalse},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSZone{}).
		WithObjects(zone, domain).
		WithIndex(&networkingv1alpha.Domain{}, "spec.domainName", func(obj client.Object) []string {
			d := obj.(*networkingv1alpha.Domain)
			if d.Spec.DomainName == "" {
				return nil
			}
			return []string{d.Spec.DomainName}
		}).
		Build()

	r := &DNSZoneReplicator{}

	if err := r.ensureDomainRef(context.Background(), fakeClient, zone); err != nil {
		t.Fatalf("ensureDomainRef: %v", err)
	}

	var stored dnsv1alpha1.DNSZone
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(zone), &stored); err != nil {
		t.Fatalf("get stored zone: %v", err)
	}
	if stored.Status.DomainRef == nil {
		t.Fatal("expected the zone to publish its Domain link while unverified")
	}
	if stored.Status.DomainRef.Name != domain.Name {
		t.Fatalf("DomainRef.Name = %q, want %q", stored.Status.DomainRef.Name, domain.Name)
	}

	if err := r.ensureDomainRef(context.Background(), fakeClient, zone); err != nil {
		t.Fatalf("ensureDomainRef (repeat): %v", err)
	}
}

func statusEqualIgnoringTransitionTime(a, b dnsv1alpha1.DNSZoneStatus) bool {
	if !reflect.DeepEqual(a.Nameservers, b.Nameservers) {
		return false
	}
	if a.RecordCount != b.RecordCount {
		return false
	}
	if !reflect.DeepEqual(a.DomainRef, b.DomainRef) {
		return false
	}
	return conditionsEqualIgnoringTransitionTime(a.Conditions, b.Conditions)
}

func conditionsEqualIgnoringTransitionTime(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	byType := make(map[string]metav1.Condition, len(b))
	for i := range b {
		byType[b[i].Type] = b[i]
	}
	for i := range a {
		other, ok := byType[a[i].Type]
		if !ok {
			return false
		}
		if a[i].Status != other.Status ||
			a[i].Reason != other.Reason ||
			a[i].Message != other.Message ||
			a[i].ObservedGeneration != other.ObservedGeneration {
			return false
		}
	}
	return true
}

// Namespaces shared by the zone-accounting tests below.
const (
	accountingTestNS     = "datum-downstream-dnszone-accounting"
	accountingTestZoneNS = "default"
)

// TestZoneAccountingSurvivesTheClusterNamePrefixChange reproduces the 2026-08-24
// staging incident: records written before multicluster-runtime v0.23 carry a
// leading slash on the cluster name, and a byte comparison parked every zone in
// the fleet on Accepted=False/DNSZoneInUse in one sweep.
func TestZoneAccountingSurvivesTheClusterNamePrefixChange(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}

	const (
		accountingNS = accountingTestNS
		domain       = "datum-staging.net"
		// What the provider produced before the upgrade, and what is on disk.
		legacyOwner = "/datum-cloud/default/datum-staging.net"
		// What the reconciler computes now.
		currentOwner = "datum-cloud/default/datum-staging.net"
	)

	existing := &corev1.ConfigMap{}
	existing.Namespace = accountingNS
	existing.Name = domain
	existing.Data = map[string]string{"owner": legacyOwner}

	ns := &corev1.Namespace{}
	ns.Name = accountingNS

	downstream := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, existing).Build()
	r := &DNSZoneReplicator{
		DownstreamClient:    downstream,
		AccountingNamespace: accountingNS,
	}

	zone := &dnsv1alpha1.DNSZone{}
	zone.Namespace = accountingTestZoneNS
	zone.Name = domain
	zone.Spec.DomainName = domain

	owned, err := r.ensureZoneAccounting(context.Background(), zone, currentOwner)
	if err != nil {
		t.Fatalf("ensureZoneAccounting: %v", err)
	}
	if !owned {
		t.Fatalf("a zone whose accounting predates the cluster-name change was reported as owned by another resource")
	}

	// Rewritten forward, so the next reconcile matches exactly rather than
	// leaning on the compatibility path forever.
	var after corev1.ConfigMap
	if err := downstream.Get(context.Background(),
		client.ObjectKey{Namespace: accountingNS, Name: domain}, &after); err != nil {
		t.Fatalf("re-reading the accounting configmap: %v", err)
	}
	if got := after.Data["owner"]; got != currentOwner {
		t.Errorf("owner after migration = %q, want %q", got, currentOwner)
	}
}

// TestZoneAccountingStillRejectsAGenuinelyDifferentOwner: the tolerance must not
// turn the ownership check into a rubber stamp.
func TestZoneAccountingStillRejectsAGenuinelyDifferentOwner(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}

	const accountingNS = accountingTestNS
	const domain = "contested.example"

	for _, stored := range []string{
		"someone-else/default/contested-example",  // different cluster
		"/someone-else/default/contested-example", // different cluster, legacy spelling
		"datum-cloud/other-ns/contested-example",  // same cluster, different namespace
		"datum-cloud/default/a-different-object",  // same cluster and namespace, other object
	} {
		t.Run(stored, func(t *testing.T) {
			cm := &corev1.ConfigMap{}
			cm.Namespace = accountingNS
			cm.Name = domain
			cm.Data = map[string]string{"owner": stored}
			ns := &corev1.Namespace{}
			ns.Name = accountingNS

			downstream := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, cm).Build()
			r := &DNSZoneReplicator{DownstreamClient: downstream, AccountingNamespace: accountingNS}

			zone := &dnsv1alpha1.DNSZone{}
			zone.Namespace = accountingTestZoneNS
			zone.Name = "contested-example"
			zone.Spec.DomainName = domain

			owned, err := r.ensureZoneAccounting(context.Background(), zone, "datum-cloud/default/contested-example")
			if err != nil {
				t.Fatalf("ensureZoneAccounting: %v", err)
			}
			if owned {
				t.Errorf("claimed a zone owned by %q", stored)
			}
			// A rejected claim must not rewrite somebody else's record.
			var after corev1.ConfigMap
			if err := downstream.Get(context.Background(),
				client.ObjectKey{Namespace: accountingNS, Name: domain}, &after); err != nil {
				t.Fatalf("re-reading: %v", err)
			}
			if got := after.Data["owner"]; got != stored {
				t.Errorf("owner was overwritten: %q, want %q untouched", got, stored)
			}
		})
	}
}

// TestCleanupReleasesAZoneWithLegacyAccounting covers the third comparison site,
// where a byte comparison leaks the record on teardown and blocks the domain
// from ever being reclaimed.
func TestCleanupReleasesAZoneWithLegacyAccounting(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}

	const accountingNS = accountingTestNS
	const domain = "going-away.example"

	cm := &corev1.ConfigMap{}
	cm.Namespace = accountingNS
	cm.Name = domain
	cm.Data = map[string]string{"owner": "/datum-cloud/default/going-away-example"}

	downstream := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	r := &DNSZoneReplicator{DownstreamClient: downstream, AccountingNamespace: accountingNS}

	zone := &dnsv1alpha1.DNSZone{}
	zone.Namespace = accountingTestZoneNS
	zone.Name = "going-away-example"
	zone.Spec.DomainName = domain

	if err := r.cleanupZoneAccounting(context.Background(), zone, "datum-cloud/default/going-away-example"); err != nil {
		t.Fatalf("cleanupZoneAccounting: %v", err)
	}

	var after corev1.ConfigMap
	err := downstream.Get(context.Background(), client.ObjectKey{Namespace: accountingNS, Name: domain}, &after)
	if err == nil {
		t.Fatalf("the accounting configmap survived teardown, so the domain stays claimed forever")
	}
}
