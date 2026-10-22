package controlplane

import (
	"context"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestResolveNamingReturnsCanonicalThenAuthorizedAliases(t *testing.T) {
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
		t.Fatal(err)
	}
	resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "project", UID: "context-a"}, Status: dnsv1alpha1.DNSResolverContextStatus{ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespaceStatus{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: "canonical"}}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}}}
	canonical := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "canonical", Namespace: "project", UID: "zone-c"}}
	custom := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "custom", Namespace: "project", UID: "zone-x"}}
	policy := &dnsv1alpha1.DNSNamingPolicy{ObjectMeta: metav1.ObjectMeta{Name: "custom", Namespace: "project", UID: "policy-a"}, Spec: dnsv1alpha1.DNSNamingPolicySpec{Priority: 10}, Status: dnsv1alpha1.DNSNamingPolicyStatus{ResolvedResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}, ResolvedAdditionalNames: []dnsv1alpha1.DNSResolvedAdditionalNameRule{{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: "custom"}, NamePrefix: "instances"}}, Conditions: []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue}}}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(resolver, canonical, custom, policy).Build()
	r := &Reconciler{Client: c, Options: Options{ProjectUID: "project-a"}}
	seedReferenceState(t, r, policy, map[string]dnsv1alpha1.DNSObjectReference{"context": {Name: resolver.Name, UID: resolver.UID}, "zone/custom": {Name: custom.Name, UID: custom.UID}})
	names, err := ResolveNaming(context.Background(), c, "project", "project-a", resolver.Name, nil, "Web 01")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0].Name != "web-01" || !names[0].Canonical || names[1].Name != "web-01.instances" || names[1].Canonical {
		t.Fatalf("unexpected names: %#v", names)
	}
}

func TestResolveNamingRequiresCurrentManagedNamespace(t *testing.T) {
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*dnsv1alpha1.DNSResolverContext){
		"managed namespace disabled": func(c *dnsv1alpha1.DNSResolverContext) {
			c.Status.ManagedNamespace = dnsv1alpha1.DNSResolverContextManagedNamespaceStatus{}
		},
		"ready for an older generation": func(c *dnsv1alpha1.DNSResolverContext) {
			c.Status.Conditions[0].ObservedGeneration = c.Generation - 1
		},
	} {
		managed := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "project", UID: "vpc-a", Generation: 2}, Status: dnsv1alpha1.DNSResolverContextStatus{ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespaceStatus{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: "canonical"}}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 2}}}}
		mutate(managed)
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(managed).Build()
		if names, err := ResolveNaming(context.Background(), c, "project", "project-a", managed.Name, nil, "web"); err == nil {
			t.Fatalf("%s: resolved %#v", name, names)
		}
	}
}
