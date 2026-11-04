// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRestartAfterInterruptedPublishPublishesAtANewRevision(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
		t.Fatal(err)
	}
	network := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "project", UID: "vpc-a"}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer-a"}}
	zone := fixture(now).Zone.DeepCopy()
	zone.Namespace = "project"
	zone.Finalizers = []string{privateZoneFinalizer}
	assoc := fixture(now).Associations[0].DeepCopy()
	assoc.Namespace = "project"
	assoc.Name = "zone-prod"
	assoc.Spec.ResolverContextRef = dnsv1alpha1.DNSNameReference{Name: "prod"}
	assoc.Status = dnsv1alpha1.DNSZoneAssociationStatus{}
	reg := fixture(now).Registrations[0].DeepCopy()
	reg.Namespace = "project"
	contrib := fixture(now).Contributions[0].DeepCopy()
	contrib.Namespace = "project"
	contrib.Generation = 1
	access := &dnsv1alpha1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "prod-east", Namespace: "project", UID: "access-east"}, Spec: dnsv1alpha1.DNSResolverAccessBindingSpec{ContextRef: dnsv1alpha1.DNSObjectReference{Name: "prod", UID: "vpc-a"}, Region: "east", DestinationAddress: "fd53::11"}}
	crash := true
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(network, zone, assoc, reg, contrib, access).
		WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSZoneAssociation{}, &dnsv1alpha1.DNSRegistration{}, &dnsv1alpha1.DNSRecordContribution{}, &dnsv1alpha1.DNSPublicationManifest{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*dnsv1alpha1.DNSPublicationManifest); ok && crash {
				return errors.New("process stopped before the manifest was written")
			}
			return c.Create(ctx, obj, opts...)
		}}).Build()
	newWriter := func() *Reconciler {
		return &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{ProjectUID: "project-a", ProjectNamespace: "project", PlatformNamespace: "internal-dns-system", Identity: "compiler-a", ServingRegions: []ServingRegion{{Region: "east", Shard: "s1"}}, AddressAllocator: testAllocator{}, Now: func() time.Time { return now }}}
	}
	reconcile := func(r *Reconciler) {
		for i := 0; i < 8; i++ {
			_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "project", Name: "project"}})
		}
	}
	ownership := func() dnsv1alpha1.DNSPublicationOwnership {
		t.Helper()
		var own dnsv1alpha1.DNSPublicationOwnership
		if err := c.Get(ctx, client.ObjectKey{Namespace: "internal-dns-system", Name: ownershipName(zone.UID)}, &own); err != nil {
			t.Fatal(err)
		}
		return own
	}

	reconcile(newWriter())
	interrupted := ownership()
	if interrupted.Spec.ActiveManifestName != "" || interrupted.Spec.NextRevision < 2 {
		t.Fatalf("the interrupted pass must have reserved a revision without activating one: %#v", interrupted.Spec)
	}

	crash = false
	reconcile(newWriter())
	recovered := ownership()
	if recovered.Spec.ActiveManifestName == "" {
		t.Fatal("restart did not publish the interrupted update")
	}
	var m dnsv1alpha1.DNSPublicationManifest
	if err := c.Get(ctx, client.ObjectKey{Namespace: "internal-dns-system", Name: recovered.Spec.ActiveManifestName}, &m); err != nil {
		t.Fatal(err)
	}
	if m.Spec.Revision < interrupted.Spec.NextRevision || m.Spec.WriterEpoch != interrupted.Spec.WriterEpoch {
		t.Fatalf("restart reused a revision reserved before the interruption: got %d, reserved below %d", m.Spec.Revision, interrupted.Spec.NextRevision)
	}
	if len(m.Spec.ContributionFences) != 1 || m.Spec.ContributionFences[0].UID != contrib.UID {
		t.Fatalf("the published revision lost the contribution: %#v", m.Spec.ContributionFences)
	}
}
