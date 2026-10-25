// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestZonePublishesToRegionsAnyContextIsBoundIn(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
		t.Fatal(err)
	}
	binding := func(name string, contextUID types.UID, region, shard string) *dnsv1alpha1.DNSResolverBinding {
		return &dnsv1alpha1.DNSResolverBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "internal-dns-system", Labels: map[string]string{referenceProjectLabel: "project-a"}},
			Spec: dnsv1alpha1.DNSResolverBindingSpec{
				Source:    dnsv1alpha1.DNSResolverBindingSource{ProjectUID: "project-a", ResolverContextRef: dnsv1alpha1.DNSObjectReference{UID: contextUID}},
				Placement: dnsv1alpha1.DNSResolverBindingPlacement{Region: region, Shard: shard},
			},
		}
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(binding("vpc-a-east", "vpc-a", "east", "s1")).Build()
	r := &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{ProjectUID: "project-a", PlatformNamespace: "internal-dns-system",
		ServingRegions: []ServingRegion{{Region: "east", Shard: "s1"}, {Region: "west", Shard: "s2"}}}}
	own := &dnsv1alpha1.DNSPublicationOwnership{}

	targets, err := r.publicationTargets(ctx, own, []types.UID{"vpc-a", "vpc-b"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != (ServingRegion{Region: "east", Shard: "s1"}) {
		t.Fatalf("a zone whose only bound context is in east must publish only to east, got %#v", targets)
	}

	targets, err = r.publicationTargets(ctx, own, []types.UID{"vpc-b"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("a zone bound nowhere keeps every assigned shard, got %#v", targets)
	}
}
