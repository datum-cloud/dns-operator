// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func verifyNamedReferencesWithAPI(t *testing.T, cl client.Client) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "named-context", Namespace: "project"}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "named-consumer"}}
	requireAccessTestSuccess(t, cl.Create(ctx, resolver))
	setCondition(&resolver.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "", resolver.Generation, now)
	requireAccessTestSuccess(t, cl.Status().Update(ctx, resolver))
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "named-zone", Namespace: "project"}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "named.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate, DNSZoneClassName: "private"}}
	requireAccessTestSuccess(t, cl.Create(ctx, zone))
	association := &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: "named-association", Namespace: "project"}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: zone.Name}, ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}}}
	policy := &dnsv1alpha1.DNSNamingPolicy{ObjectMeta: metav1.ObjectMeta{Name: "named-policy", Namespace: "project"}, Spec: dnsv1alpha1.DNSNamingPolicySpec{ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}, AdditionalNames: []dnsv1alpha1.DNSAdditionalNameRule{{Selector: &dnsv1alpha1.DNSRegistrationSelector{MatchLabels: map[string]string{"app": "storefront"}}, DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: zone.Name}, NamePrefix: "hosts"}}}}
	requireAccessTestSuccess(t, cl.Create(ctx, association))
	requireAccessTestSuccess(t, cl.Create(ctx, policy))
	r := &Reconciler{Client: cl, Options: Options{PlatformNamespace: "platform"}}
	reconcile := func(want metav1.ConditionStatus) {
		t.Helper()
		requireAccessTestSuccess(t, cl.Get(ctx, client.ObjectKeyFromObject(association), association))
		associations := []dnsv1alpha1.DNSZoneAssociation{*association}
		zones := map[string]*dnsv1alpha1.DNSZone{zone.Name: zone}
		requireAccessTestSuccess(t, r.reconcileAssociations(ctx, associations, zones, now))
		requireAccessTestSuccess(t, cl.Get(ctx, client.ObjectKeyFromObject(policy), policy))
		requireAccessTestSuccess(t, r.reconcileNamingPolicies(ctx, []dnsv1alpha1.DNSNamingPolicy{*policy}, associations, zones, now))
		requireAccessTestSuccess(t, cl.Get(ctx, client.ObjectKeyFromObject(association), association))
		requireAccessTestSuccess(t, cl.Get(ctx, client.ObjectKeyFromObject(policy), policy))
		for name, conditions := range map[string][]metav1.Condition{"association": association.Status.Conditions, "policy": policy.Status.Conditions} {
			if condition := apimeta.FindStatusCondition(conditions, "Accepted"); condition == nil || condition.Status != want {
				t.Fatalf("%s expected %s: %v", name, want, condition)
			}
		}
	}
	reconcile(metav1.ConditionTrue)
	if selector := policy.Status.ResolvedAdditionalNames[0].Selector; selector == nil || selector.MatchLabels["app"] != "storefront" {
		t.Fatal("API did not preserve the registration label selector")
	}
	originalUID := zone.UID
	requireAccessTestSuccess(t, cl.Delete(ctx, zone))
	zone = &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: zone.Name, Namespace: "project"}, Spec: zone.Spec}
	requireAccessTestSuccess(t, cl.Create(ctx, zone))
	if zone.UID == originalUID {
		t.Fatal("replacement did not receive a new API-assigned UID")
	}
	reconcile(metav1.ConditionFalse)
	associationPins, err := r.readReferences(ctx, association)
	requireAccessTestSuccess(t, err)
	policyPins, err := r.readReferences(ctx, policy)
	requireAccessTestSuccess(t, err)
	if associationPins["zone"].UID != originalUID || policyPins["zone/"+zone.Name].UID != originalUID {
		t.Fatal("API-backed replacement transferred lifetime pins")
	}
}
