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
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	managed := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "project", UID: "vpc-a"}, Status: dnsv1alpha1.DNSResolverContextStatus{ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespaceStatus{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: "canonical", UID: "zone-c"}}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}}}
	policy := &dnsv1alpha1.DNSNamingPolicy{ObjectMeta: metav1.ObjectMeta{Name: "custom", Namespace: "project"}, Spec: dnsv1alpha1.DNSNamingPolicySpec{Priority: 10}, Status: dnsv1alpha1.DNSNamingPolicyStatus{ResolvedResolverContextRef: dnsv1alpha1.DNSObjectReference{Name: "prod", UID: "vpc-a"}, ResolvedAdditionalNames: []dnsv1alpha1.DNSResolvedAdditionalNameRule{{Selector: nil, DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: "custom", UID: "zone-x"}, NamePrefix: "instances"}}, Conditions: []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue}}}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(managed, policy).Build()
	names, err := ResolveNaming(context.Background(), c, "project", "project-a", "vpc-a", nil, "Web 01")
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
		managed := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "project", UID: "vpc-a", Generation: 2}, Status: dnsv1alpha1.DNSResolverContextStatus{ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespaceStatus{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: "canonical", UID: "zone-c"}}, Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 2}}}}
		mutate(managed)
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(managed).Build()
		if names, err := ResolveNaming(context.Background(), c, "project", "project-a", "vpc-a", nil, "web"); err == nil {
			t.Fatalf("%s: resolved %#v", name, names)
		}
	}
}
