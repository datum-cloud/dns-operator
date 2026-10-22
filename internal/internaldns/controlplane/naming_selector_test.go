// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"reflect"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNamingSelectorsMatchRegistrationLabelsWithinContext(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := addReferenceStateToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "consumer-a", Namespace: "project", UID: "context-a"}, Status: dnsv1alpha1.DNSResolverContextStatus{ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespaceStatus{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: "canonical"}}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}}}
	canonical := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "canonical", Namespace: "project", UID: "canonical-zone"}}
	custom := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "custom", Namespace: "project", UID: "custom-zone"}}
	zone := dnsv1alpha1.DNSNameReference{Name: custom.Name}
	storefront := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "storefront"}}
	policy := &dnsv1alpha1.DNSNamingPolicy{ObjectMeta: metav1.ObjectMeta{Name: "names", Namespace: "project", UID: "names-uid", Generation: 1}, Status: dnsv1alpha1.DNSNamingPolicyStatus{ResolvedResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}, Conditions: []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue, ObservedGeneration: 1}}, ResolvedAdditionalNames: []dnsv1alpha1.DNSResolvedAdditionalNameRule{
		{DNSZoneRef: zone, NamePrefix: "all"},
		{Selector: &metav1.LabelSelector{}, DNSZoneRef: zone, NamePrefix: "empty"},
		{Selector: storefront, DNSZoneRef: zone, NamePrefix: "storefront"},
		{Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"storefront"}}}}, DNSZoneRef: zone, NamePrefix: "storefront"},
		{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "storefront"}, MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "tier", Operator: metav1.LabelSelectorOpIn, Values: []string{"backend", "worker"}}, {Key: "disabled", Operator: metav1.LabelSelectorOpDoesNotExist}}}, DNSZoneRef: zone, NamePrefix: "backend"},
	}}}
	other := policy.DeepCopy()
	other.Name = "other-context"
	other.Status.ResolvedResolverContextRef.Name = "consumer-b"
	other.Status.ResolvedAdditionalNames = []dnsv1alpha1.DNSResolvedAdditionalNameRule{{DNSZoneRef: zone, NamePrefix: "other-context"}}
	stale := policy.DeepCopy()
	stale.Name = "stale-policy"
	stale.Generation++
	stale.Status.ResolvedAdditionalNames = []dnsv1alpha1.DNSResolvedAdditionalNameRule{{DNSZoneRef: zone, NamePrefix: "stale"}}
	foreign := policy.DeepCopy()
	foreign.Name = "other-namespace"
	foreign.Namespace = "other-project"
	foreign.Status.ResolvedAdditionalNames = []dnsv1alpha1.DNSResolvedAdditionalNameRule{{DNSZoneRef: zone, NamePrefix: "foreign"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(resolver, canonical, custom, policy, other, stale, foreign).Build()
	r := &Reconciler{Client: cl, Options: Options{ProjectUID: "project-a"}}
	seedReferenceState(t, r, policy, map[string]dnsv1alpha1.DNSObjectReference{"context": {Name: resolver.Name, UID: resolver.UID}, "zone/custom": {Name: custom.Name, UID: custom.UID}})
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   []string
	}{
		{"unlabeled", nil, []string{"web-01", "web-01.all", "web-01.empty"}},
		{"matching publisher one", map[string]string{"app": "storefront", "publisher": "one"}, []string{"web-01", "web-01.all", "web-01.empty", "web-01.storefront"}},
		{"matching publisher two", map[string]string{"app": "storefront", "publisher": "two"}, []string{"web-01", "web-01.all", "web-01.empty", "web-01.storefront"}},
		{"different app", map[string]string{"app": "billing"}, []string{"web-01", "web-01.all", "web-01.empty"}},
		{"set expression", map[string]string{"app": "storefront", "tier": "worker"}, []string{"web-01", "web-01.all", "web-01.empty", "web-01.storefront", "web-01.backend"}},
		{"excluded label", map[string]string{"app": "storefront", "tier": "worker", "disabled": "true"}, []string{"web-01", "web-01.all", "web-01.empty", "web-01.storefront"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names, err := ResolveNaming(context.Background(), cl, "project", "project-a", "consumer-a", tc.labels, "Web 01")
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, len(names))
			for i, n := range names {
				got[i] = n.Name
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			if !names[0].Canonical {
				t.Fatal("default name was not retained")
			}
		})
	}
	t.Run("malformed accepted selector", func(t *testing.T) {
		if err := cl.Get(context.Background(), client.ObjectKeyFromObject(policy), policy); err != nil {
			t.Fatal(err)
		}
		policy.Status.ResolvedAdditionalNames[0].Selector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: "Unsupported"}}}
		if err := cl.Update(context.Background(), policy); err != nil {
			t.Fatal(err)
		}
		names, err := ResolveNaming(context.Background(), cl, "project", "project-a", "consumer-a", nil, "Web 01")
		if err == nil || len(names) != 0 {
			t.Fatalf("malformed accepted selector produced names: %v, %v", names, err)
		}
	})
}

func TestNamingPolicyValidatesSelectorsAndDuplicateRules(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	scheme := runtime.NewScheme()
	if err := addReferenceStateToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "context", Namespace: "project", UID: "context-uid", Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer"}}
	setCondition(&resolver.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "", 1, now)
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", UID: "zone-uid", Generation: 1}, Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	association := dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: "association", Namespace: "project", UID: "association-uid", Generation: 1}, Status: dnsv1alpha1.DNSZoneAssociationStatus{ResolvedDNSZoneRef: dnsv1alpha1.DNSNameReference{Name: zone.Name}, ResolvedResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}}}
	setCondition(&association.Status.Conditions, "Accepted", metav1.ConditionTrue, "Accepted", "", 1, now)
	for _, tc := range []struct {
		name      string
		selectors []*dnsv1alpha1.DNSRegistrationSelector
		valid     bool
	}{
		{"omitted", []*dnsv1alpha1.DNSRegistrationSelector{nil}, true},
		{"empty", []*dnsv1alpha1.DNSRegistrationSelector{{}}, true},
		{"labels", []*dnsv1alpha1.DNSRegistrationSelector{{MatchLabels: map[string]string{"app": "storefront"}}}, true},
		{"invalid-key", []*dnsv1alpha1.DNSRegistrationSelector{{MatchLabels: map[string]string{"bad key": "value"}}}, false},
		{"invalid-operator", []*dnsv1alpha1.DNSRegistrationSelector{{MatchExpressions: []dnsv1alpha1.DNSRegistrationSelectorRequirement{{Key: "app", Operator: "Unsupported"}}}}, false},
		{"missing-values", []*dnsv1alpha1.DNSRegistrationSelector{{MatchExpressions: []dnsv1alpha1.DNSRegistrationSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpIn}}}}, false},
		{"duplicate", []*dnsv1alpha1.DNSRegistrationSelector{nil, {}}, false},
		{"overlapping", []*dnsv1alpha1.DNSRegistrationSelector{nil, {MatchLabels: map[string]string{"app": "storefront"}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := &dnsv1alpha1.DNSNamingPolicy{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: "project", Generation: 1}, Spec: dnsv1alpha1.DNSNamingPolicySpec{ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}}}
			for _, selector := range tc.selectors {
				policy.Spec.AdditionalNames = append(policy.Spec.AdditionalNames, dnsv1alpha1.DNSAdditionalNameRule{Selector: selector, DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: zone.Name}, NamePrefix: "application"})
			}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(resolver, policy).WithStatusSubresource(policy).Build()
			r := &Reconciler{Client: cl}
			seedReferenceState(t, r, &association, map[string]dnsv1alpha1.DNSObjectReference{"context": {Name: resolver.Name, UID: resolver.UID}, "zone": {Name: zone.Name, UID: zone.UID, Generation: zone.Generation}})
			if err := r.reconcileNamingPolicies(ctx, []dnsv1alpha1.DNSNamingPolicy{*policy}, []dnsv1alpha1.DNSZoneAssociation{association}, map[string]*dnsv1alpha1.DNSZone{zone.Name: zone}, now); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
				t.Fatal(err)
			}
			condition := apimeta.FindStatusCondition(policy.Status.Conditions, "Accepted")
			if condition == nil || (condition.Status == metav1.ConditionTrue) != tc.valid {
				t.Fatalf("accepted=%v want %v", condition, tc.valid)
			}
			pins, err := r.readReferences(ctx, policy)
			if err != nil || pins["zone/"+zone.Name].UID != zone.UID {
				t.Fatal("selector validation lost the zone lifetime pin")
			}
		})
	}
}
