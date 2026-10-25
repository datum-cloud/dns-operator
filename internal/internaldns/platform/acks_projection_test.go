// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"errors"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestOneSinkProjectsEachManifest(t *testing.T) {
	regions := map[string][]model.Member{"central/s": nil, "east/s": nil}
	central := &AckSink{Region: "central", Shard: "s", PublicationRegions: regions}
	east := &AckSink{Region: "east", Shard: "s", PublicationRegions: regions}
	manifest := func(targets ...string) *dnsv1alpha1.DNSPublicationManifest {
		m := &dnsv1alpha1.DNSPublicationManifest{}
		for _, region := range targets {
			m.Spec.ServingTargets = append(m.Spec.ServingTargets, dnsv1alpha1.DNSPublicationTarget{Region: region, Shard: "s"})
		}
		return m
	}
	for _, tc := range []struct {
		name          string
		manifest      *dnsv1alpha1.DNSPublicationManifest
		central, east bool
	}{
		{"both regions", manifest("east", "central"), true, false},
		{"east only", manifest("east"), false, true},
		{"no targets", manifest(), true, false},
		{"unconfigured region first", manifest("alpha", "east"), false, true},
	} {
		if got := central.projectsManifest(tc.manifest); got != tc.central {
			t.Errorf("%s: central projects = %v, want %v", tc.name, got, tc.central)
		}
		if got := east.projectsManifest(tc.manifest); got != tc.east {
			t.Errorf("%s: east projects = %v, want %v", tc.name, got, tc.east)
		}
	}

	single := &AckSink{Region: "central", Shard: "s"}
	if !single.projectsManifest(manifest("central")) {
		t.Error("a sole sink projects every manifest")
	}
}

func TestRefreshWithdrawsPublicationWhenItsShardFails(t *testing.T) {
	ctx := context.Background()
	m := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "zone-1", Namespace: "platform", Generation: 1}}
	m.Spec.Revision = 1
	m.Spec.ServingTargets = []dnsv1alpha1.DNSPublicationTarget{{Region: "central", Shard: "s"}}
	m.Status.Conditions = []metav1.Condition{{Type: "Published", Status: metav1.ConditionTrue, Reason: "ReplicaVerification", ObservedGeneration: 1, LastTransitionTime: metav1.Now()}}
	owner := &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "zone-1", Namespace: "platform"}}
	owner.Spec.ActiveManifestName = "zone-1"
	base := testClient(t, m, owner)
	cl := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*dnsv1alpha1.DNSResolverBindingList); ok {
				return errors.New("binding list unavailable")
			}
			return c.List(ctx, list, opts...)
		},
	})

	sink := &AckSink{Client: cl, Namespace: "platform", Region: "central", Shard: "s",
		PublicationRegions: map[string][]model.Member{"central/s": {{MemberID: "regional-0", ReplicaID: "regional-0", Role: "regional"}}}}
	if err := sink.Refresh(ctx); err == nil {
		t.Fatal("a failing shard must still report its error")
	}
	var got dnsv1alpha1.DNSPublicationManifest
	if err := cl.Get(ctx, client.ObjectKeyFromObject(m), &got); err != nil {
		t.Fatal(err)
	}
	if c := apimeta.FindStatusCondition(got.Status.Conditions, "Published"); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("publication without acknowledgements must be withdrawn even when the shard's own state is unavailable, got %#v", c)
	}
}
