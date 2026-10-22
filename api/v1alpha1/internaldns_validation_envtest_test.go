// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// These tests exercise the served CRD schemas without an admission webhook.
func TestPrivateDNSValidationAtAPIServer(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS to test private DNS schemas against the API server")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "validation"}}); err != nil {
		t.Fatal(err)
	}
	validateResolverContextBoundaries(t, cl)
	requireInvalid := func(t *testing.T, err error) {
		t.Helper()
		if !apierrors.IsInvalid(err) {
			t.Fatalf("expected API validation rejection, got %v", err)
		}
	}
	newAccess := func(name string) *DNSResolverAccessBinding {
		return &DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "validation"}, Spec: DNSResolverAccessBindingSpec{ContextRef: DNSObjectReference{Name: "context", UID: "context-uid"}, Region: "central", DestinationAddress: "fd53::1"}}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*DNSResolverAccessBinding)
	}{
		{"empty-context", func(a *DNSResolverAccessBinding) { a.Spec.ContextRef.Name = "" }},
		{"missing-context-uid", func(a *DNSResolverAccessBinding) { a.Spec.ContextRef.UID = "" }},
		{"empty-region", func(a *DNSResolverAccessBinding) { a.Spec.Region = "" }},
		{"empty-address", func(a *DNSResolverAccessBinding) { a.Spec.DestinationAddress = "" }},
		{"invalid-address", func(a *DNSResolverAccessBinding) { a.Spec.DestinationAddress = "resolver.internal" }},
	} {
		t.Run(tc.name, func(t *testing.T) { a := newAccess(tc.name); tc.mutate(a); requireInvalid(t, cl.Create(ctx, a)) })
	}
	for _, tc := range []struct {
		name   string
		mutate func(*DNSResolverAccessBinding)
	}{
		{"change-context", func(a *DNSResolverAccessBinding) { a.Spec.ContextRef.Name = "other" }},
		{"change-region", func(a *DNSResolverAccessBinding) { a.Spec.Region = "east" }},
		{"change-address", func(a *DNSResolverAccessBinding) { a.Spec.DestinationAddress = "fd53::2" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAccess(tc.name)
			if err := cl.Create(ctx, a); err != nil {
				t.Fatal(err)
			}
			tc.mutate(a)
			requireInvalid(t, cl.Update(ctx, a))
		})
	}
	t.Run("metadata-and-status", func(t *testing.T) {
		a := newAccess("renewal")
		if err := cl.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := cl.Update(ctx, a); err != nil {
			t.Fatal(err)
		}
		a.Status.ObservedGeneration = a.Generation
		if err := cl.Status().Update(ctx, a); err != nil {
			t.Fatalf("access status update failed: %v", err)
		}
		a.Labels = map[string]string{"test": "metadata"}
		if err := cl.Update(ctx, a); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("consumer-identity", func(t *testing.T) {
		a := &DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "validation"}, Spec: DNSResolverContextSpec{ConsumerID: ""}}
		requireInvalid(t, cl.Create(ctx, a))
		a.Spec.ConsumerID = "network/application"
		if err := cl.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		a.Spec.ConsumerID = "network/other"
		requireInvalid(t, cl.Update(ctx, a))
	})
	for _, ttl := range []int32{0, 31, 2147483647} {
		t.Run("ttl", func(t *testing.T) {
			a := &DNSRegistration{ObjectMeta: metav1.ObjectMeta{GenerateName: "registration-", Namespace: "validation"}, Spec: DNSRegistrationSpec{DNSZoneRef: DNSObjectReference{Name: "zone", UID: "zone-uid"}, Name: "web", RecordTypes: []RRType{RRTypeAAAA}, TTLSeconds: ttl}}
			requireInvalid(t, cl.Create(ctx, a))
		})
	}
	t.Run("contribution-registration", func(t *testing.T) {
		a := &DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "contribution", Namespace: "validation"}, Spec: DNSRecordContributionSpec{RegistrationRef: DNSObjectReference{Name: "web", UID: "registration-uid", Generation: 1}, RecordSets: []DNSContributionRecordSet{}}}
		if err := cl.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		a.Spec.RegistrationRef.Generation = 2
		if err := cl.Update(ctx, a); err != nil {
			t.Fatalf("registration generation cannot advance: %v", err)
		}
		a.Spec.RegistrationRef.Name = "other"
		requireInvalid(t, cl.Update(ctx, a))
	})
	t.Run("atomic-contribution-and-stale-update", func(t *testing.T) {
		a := &DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "observations", Namespace: "validation"}, Spec: DNSRecordContributionSpec{RegistrationRef: DNSObjectReference{Name: "web", UID: "registration-uid", Generation: 1}, RecordSets: []DNSContributionRecordSet{}, Eligible: true}}
		if err := cl.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		stale := a.DeepCopy()
		generation := a.Generation
		a.Spec.Eligible = false
		a.Spec.Reason = "EndpointUnhealthy"
		a.Spec.RecordSets = []DNSContributionRecordSet{{RecordType: RRTypeA, Records: []RecordEntry{{Name: "web", A: &ARecordSpec{Content: "10.0.0.9"}}}}}
		if err := cl.Update(ctx, a); err != nil {
			t.Fatal(err)
		}
		if a.Generation != generation+1 {
			t.Fatalf("atomic desired state did not advance generation: %d", a.Generation)
		}
		if err := cl.Update(ctx, stale); !apierrors.IsConflict(err) {
			t.Fatalf("stale writer was not rejected: %v", err)
		}
		a.Status.ObservedGeneration = a.Generation
		a.Status.Conditions = []metav1.Condition{{Type: "Published", Status: metav1.ConditionTrue, Reason: "Applied", LastTransitionTime: metav1.Now()}}
		if err := cl.Status().Update(ctx, a); err != nil {
			t.Fatal(err)
		}
		var observed DNSRecordContribution
		if err := cl.Get(ctx, client.ObjectKeyFromObject(a), &observed); err != nil {
			t.Fatal(err)
		}
		if observed.Spec.Eligible || observed.Spec.RecordSets[0].Records[0].A.Content != "10.0.0.9" {
			t.Fatal("status write changed producer desired state")
		}
	})
	validateReferenceIdentity(t, cl, requireInvalid)
	validateRegistrationBounds(t, cl, requireInvalid)
}

// References may advance a pinned generation, but never retarget another object.
func validateReferenceIdentity(t *testing.T, cl client.Client, requireInvalid func(*testing.T, error)) {
	ctx := context.Background()
	zone := DNSObjectReference{Name: "zone", UID: "zone-uid", Generation: 1}
	resolverContext := DNSObjectReference{Name: "context", UID: "context-uid"}
	t.Run("association-reference-identity", func(t *testing.T) {
		for name, mutate := range map[string]func(*DNSZoneAssociation){
			"zone-name":    func(a *DNSZoneAssociation) { a.Spec.DNSZoneRef.Name = "other" },
			"zone-uid":     func(a *DNSZoneAssociation) { a.Spec.DNSZoneRef.UID = "other-uid" },
			"context-name": func(a *DNSZoneAssociation) { a.Spec.ResolverContextRef.Name = "other" },
			"context-uid":  func(a *DNSZoneAssociation) { a.Spec.ResolverContextRef.UID = "other-uid" },
		} {
			a := &DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{GenerateName: "association-", Namespace: "validation"}, Spec: DNSZoneAssociationSpec{DNSZoneRef: zone, ResolverContextRef: resolverContext}}
			if err := cl.Create(ctx, a); err != nil {
				t.Fatal(err)
			}
			a.Spec.DNSZoneRef.Generation = 2
			if err := cl.Update(ctx, a); err != nil {
				t.Fatalf("zone generation cannot advance: %v", err)
			}
			mutate(a)
			t.Run(name, func(t *testing.T) { requireInvalid(t, cl.Update(ctx, a)) })
		}
	})
	t.Run("naming-policy-reference-identity", func(t *testing.T) {
		for name, mutate := range map[string]func(*DNSNamingPolicy){
			"zone-name":    func(p *DNSNamingPolicy) { p.Spec.DNSZoneRef.Name = "other" },
			"zone-uid":     func(p *DNSNamingPolicy) { p.Spec.DNSZoneRef.UID = "other-uid" },
			"zone-cleared": func(p *DNSNamingPolicy) { p.Spec.DNSZoneRef = DNSObjectReference{} },
			"context-name": func(p *DNSNamingPolicy) { p.Spec.ResolverContextRef.Name = "other" },
			"context-uid":  func(p *DNSNamingPolicy) { p.Spec.ResolverContextRef.UID = "other-uid" },
		} {
			p := &DNSNamingPolicy{ObjectMeta: metav1.ObjectMeta{GenerateName: "naming-", Namespace: "validation"}, Spec: DNSNamingPolicySpec{ResolverContextRef: resolverContext, DNSZoneRef: zone, AdditionalNames: []DNSAdditionalNameRule{}}}
			if err := cl.Create(ctx, p); err != nil {
				t.Fatal(err)
			}
			p.Spec.Priority = 10
			if err := cl.Update(ctx, p); err != nil {
				t.Fatalf("naming policy cannot change unpinned fields: %v", err)
			}
			mutate(p)
			t.Run(name, func(t *testing.T) { requireInvalid(t, cl.Update(ctx, p)) })
		}
		p := &DNSNamingPolicy{ObjectMeta: metav1.ObjectMeta{GenerateName: "naming-", Namespace: "validation"}, Spec: DNSNamingPolicySpec{ResolverContextRef: resolverContext, AdditionalNames: []DNSAdditionalNameRule{}}}
		if err := cl.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		p.Spec.DNSZoneRef = zone
		t.Run("zone-selected-later", func(t *testing.T) { requireInvalid(t, cl.Update(ctx, p)) })
	})
	t.Run("registration-identity", func(t *testing.T) {
		for name, mutate := range map[string]func(*DNSRegistration){
			"name":      func(r *DNSRegistration) { r.Spec.Name = "api" },
			"zone-name": func(r *DNSRegistration) { r.Spec.DNSZoneRef.Name = "other" },
			"zone-uid":  func(r *DNSRegistration) { r.Spec.DNSZoneRef.UID = "other-uid" },
		} {
			r := &DNSRegistration{ObjectMeta: metav1.ObjectMeta{GenerateName: "registration-", Namespace: "validation"}, Spec: DNSRegistrationSpec{DNSZoneRef: zone, Name: "web", RecordTypes: []RRType{RRTypeAAAA}, TTLSeconds: 30}}
			if err := cl.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
			r.Spec.TTLSeconds = 10
			r.Spec.DNSZoneRef.Generation = 2
			if err := cl.Update(ctx, r); err != nil {
				t.Fatalf("registration cannot change TTL or zone generation: %v", err)
			}
			mutate(r)
			t.Run(name, func(t *testing.T) { requireInvalid(t, cl.Update(ctx, r)) })
		}
	})
}

func validateRegistrationBounds(t *testing.T, cl client.Client, requireInvalid func(*testing.T, error)) {
	ctx := context.Background()
	many := func(n int, prefix string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = prefix + string(rune('a'+i%26)) + string(rune('a'+i/26))
		}
		return out
	}
	for name, mutate := range map[string]func(*DNSRegistrationSpec){
		"empty-name":            func(s *DNSRegistrationSpec) { s.Name = "" },
		"name-with-space":       func(s *DNSRegistrationSpec) { s.Name = "web app" },
		"long-name":             func(s *DNSRegistrationSpec) { s.Name = strings.Repeat("a", 254) },
		"no-record-types":       func(s *DNSRegistrationSpec) { s.RecordTypes = nil },
		"duplicate-record-type": func(s *DNSRegistrationSpec) { s.RecordTypes = []RRType{RRTypeA, RRTypeA} },
		"many-reserved":         func(s *DNSRegistrationSpec) { s.ReservedDescendants = many(65, "r") },
	} {
		t.Run("registration-bounds-"+name, func(t *testing.T) {
			r := &DNSRegistration{ObjectMeta: metav1.ObjectMeta{GenerateName: "bounded-", Namespace: "validation"}, Spec: DNSRegistrationSpec{DNSZoneRef: DNSObjectReference{Name: "zone", UID: "zone-uid"}, Name: "web", RecordTypes: []RRType{RRTypeA}, TTLSeconds: 30}}
			mutate(&r.Spec)
			requireInvalid(t, cl.Create(ctx, r))
		})
	}
	t.Run("contribution-bounds", func(t *testing.T) {
		records := make([]RecordEntry, 257)
		for i := range records {
			records[i] = RecordEntry{Name: "web", A: &ARecordSpec{Content: "10.0.0.1"}}
		}
		c := &DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{GenerateName: "bounded-", Namespace: "validation"}, Spec: DNSRecordContributionSpec{RegistrationRef: DNSObjectReference{Name: "web", UID: "registration-uid", Generation: 1}, RecordSets: []DNSContributionRecordSet{{RecordType: RRTypeA, Records: records}}}}
		requireInvalid(t, cl.Create(ctx, c))
	})
}
