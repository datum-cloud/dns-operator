// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"
	"os"
	"path/filepath"
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

}
