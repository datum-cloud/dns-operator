// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNamedReferencesPreserveLifetimesAcrossReplacement(t *testing.T) {
	for _, replace := range []string{"zone", "context"} {
		t.Run(replace, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			scheme := runtime.NewScheme()
			if err := addReferenceStateToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "application", Namespace: "project", UID: "context-original", Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer"}}
			setCondition(&resolver.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "", resolver.Generation, now)
			zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "project", UID: "zone-original", Generation: 1}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "prod.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
			association := &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "project", UID: "source-original", Generation: 1}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: zone.Name}, ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}}}
			policy := &dnsv1alpha1.DNSNamingPolicy{ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "project", UID: "source-original", Generation: 1}, Spec: dnsv1alpha1.DNSNamingPolicySpec{ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}, AdditionalNames: []dnsv1alpha1.DNSAdditionalNameRule{{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: zone.Name}, NamePrefix: "hosts"}}}}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(resolver, zone, association, policy).WithStatusSubresource(association, policy).Build()
			r := &Reconciler{Client: cl}
			references := func(source client.Object) map[string]dnsv1alpha1.DNSObjectReference {
				t.Helper()
				refs, err := r.readReferences(ctx, source)
				if err != nil {
					t.Fatal(err)
				}
				return refs
			}
			zones := map[string]*dnsv1alpha1.DNSZone{zone.Name: zone}
			reconcile := func() {
				t.Helper()
				if err := cl.Get(ctx, client.ObjectKeyFromObject(association), association); err != nil {
					t.Fatal(err)
				}
				associations := []dnsv1alpha1.DNSZoneAssociation{*association}
				if err := r.reconcileAssociations(ctx, associations, zones, now); err != nil {
					t.Fatal(err)
				}
				if err := cl.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
					t.Fatal(err)
				}
				if err := r.reconcileNamingPolicies(ctx, []dnsv1alpha1.DNSNamingPolicy{*policy}, associations, zones, now); err != nil {
					t.Fatal(err)
				}
				if err := cl.Get(ctx, client.ObjectKeyFromObject(association), association); err != nil {
					t.Fatal(err)
				}
				if err := cl.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
					t.Fatal(err)
				}
			}
			assertAccepted := func(want bool) {
				t.Helper()
				for name, conditions := range map[string][]metav1.Condition{"association": association.Status.Conditions, "policy": policy.Status.Conditions} {
					condition := apimeta.FindStatusCondition(conditions, "Accepted")
					if condition == nil || (condition.Status == metav1.ConditionTrue) != want {
						t.Fatalf("%s accepted=%v, want %v", name, condition, want)
					}
				}
			}
			reconcile()
			assertAccepted(true)
			if references(association)["zone"].UID != zone.UID || references(policy)["context"].UID != resolver.UID {
				t.Fatal("name-only references did not pin lifetimes")
			}
			zone.Generation++
			reconcile()
			assertAccepted(true)
			if references(association)["zone"].Generation != zone.Generation || references(policy)["zone/"+zone.Name].Generation != zone.Generation {
				t.Fatal("resolved generations were not persisted for the same zone lifetime")
			}
			originalZoneUID := zone.UID
			originalContextUID := resolver.UID
			var replaced client.Object = zone
			if replace == "context" {
				replaced = resolver
			}
			if err := cl.Delete(ctx, replaced); err != nil {
				t.Fatal(err)
			}
			if replace == "zone" {
				delete(zones, zone.Name)
			}
			reconcile()
			assertAccepted(false)
			if references(association)["zone"].UID != originalZoneUID || references(policy)["context"].UID != originalContextUID {
				t.Fatal("deletion cleared lifetime pins")
			}
			replaced.SetResourceVersion("")
			replaced.SetUID("replacement")
			if err := cl.Create(ctx, replaced); err != nil {
				t.Fatal(err)
			}
			if replace == "zone" {
				zones[zone.Name] = zone
			}
			reconcile()
			assertAccepted(false)
			if references(association)["zone"].UID != originalZoneUID || references(policy)["zone/"+zone.Name].UID != originalZoneUID || references(policy)["context"].UID != originalContextUID {
				t.Fatal("replacement inherited lifetime pins")
			}
			// Recreate both customer resources to explicitly attach the replacement.
			if err := cl.Delete(ctx, association); err != nil {
				t.Fatal(err)
			}
			association.ResourceVersion = ""
			association.UID = "association-recreated"
			association.Status = dnsv1alpha1.DNSZoneAssociationStatus{}
			if err := cl.Create(ctx, association); err != nil {
				t.Fatal(err)
			}
			reconcile()
			if condition := apimeta.FindStatusCondition(policy.Status.Conditions, "Accepted"); condition == nil || condition.Status != metav1.ConditionFalse {
				t.Fatal("reattaching an association silently rebound the policy")
			}
			if err := cl.Delete(ctx, policy); err != nil {
				t.Fatal(err)
			}
			policy.ResourceVersion = ""
			policy.UID = "policy-recreated"
			policy.Status = dnsv1alpha1.DNSNamingPolicyStatus{}
			if err := cl.Create(ctx, policy); err != nil {
				t.Fatal(err)
			}
			reconcile()
			assertAccepted(true)
		})
	}
}
