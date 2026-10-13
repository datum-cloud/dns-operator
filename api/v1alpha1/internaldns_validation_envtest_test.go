// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
		return &DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "validation"}, Spec: DNSResolverAccessBindingSpec{
			ContextRef: DNSObjectReference{Name: "context", UID: "context-uid"}, Region: "central", QueryIdentity: DNSResolverQueryIdentity{Type: DNSResolverQueryIdentityDestinationAddress, Value: "fd53::1"}, Port: 53, Transports: []string{"UDP", "TCP"}, Authorization: DNSResolverAccessAuthorization{WriterEpoch: 1, Sequence: 1, ValidUntil: metav1.NewTime(time.Now().Add(time.Minute))},
		}}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*DNSResolverAccessBinding)
	}{
		{"empty-context", func(a *DNSResolverAccessBinding) { a.Spec.ContextRef.Name = "" }},
		{"missing-context-uid", func(a *DNSResolverAccessBinding) { a.Spec.ContextRef.UID = "" }},
		{"empty-region", func(a *DNSResolverAccessBinding) { a.Spec.Region = "" }},
		{"empty-address", func(a *DNSResolverAccessBinding) { a.Spec.QueryIdentity.Value = "" }},
		{"unsupported-identity", func(a *DNSResolverAccessBinding) { a.Spec.QueryIdentity.Type = "SourceAddress" }},
		{"wrong-port", func(a *DNSResolverAccessBinding) { a.Spec.Port = 5353 }},
		{"missing-tcp", func(a *DNSResolverAccessBinding) { a.Spec.Transports = []string{"UDP"} }},
		{"duplicate-transport", func(a *DNSResolverAccessBinding) { a.Spec.Transports = []string{"UDP", "UDP"} }},
		{"unsupported-transport", func(a *DNSResolverAccessBinding) { a.Spec.Transports = []string{"UDP", "TLS"} }},
		{"zero-epoch", func(a *DNSResolverAccessBinding) { a.Spec.Authorization.WriterEpoch = 0 }},
		{"zero-sequence", func(a *DNSResolverAccessBinding) { a.Spec.Authorization.Sequence = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) { a := newAccess(tc.name); tc.mutate(a); requireInvalid(t, cl.Create(ctx, a)) })
	}
	for _, tc := range []struct {
		name   string
		mutate func(*DNSResolverAccessBinding)
	}{
		{"change-context", func(a *DNSResolverAccessBinding) { a.Spec.ContextRef.Name = "other" }},
		{"change-region", func(a *DNSResolverAccessBinding) { a.Spec.Region = "east" }},
		{"change-address", func(a *DNSResolverAccessBinding) { a.Spec.QueryIdentity.Value = "fd53::2" }},
		{"change-epoch", func(a *DNSResolverAccessBinding) { a.Spec.Authorization.WriterEpoch++ }},
		{"extend-without-sequence", func(a *DNSResolverAccessBinding) {
			a.Spec.Authorization.ValidUntil = metav1.NewTime(a.Spec.Authorization.ValidUntil.Add(time.Minute))
		}},
		{"decrease-sequence", func(a *DNSResolverAccessBinding) { a.Spec.Authorization.Sequence = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAccess(tc.name)
			a.Spec.Authorization.Sequence = 2
			if err := cl.Create(ctx, a); err != nil {
				t.Fatal(err)
			}
			tc.mutate(a)
			requireInvalid(t, cl.Update(ctx, a))
		})
	}
	t.Run("renewal-and-status", func(t *testing.T) {
		a := newAccess("renewal")
		if err := cl.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		a.Spec.Authorization.Sequence++
		a.Spec.Authorization.ValidUntil = metav1.NewTime(a.Spec.Authorization.ValidUntil.Add(time.Minute))
		if err := cl.Update(ctx, a); err != nil {
			t.Fatal(err)
		}
		a.Status.ObservedSequence = a.Spec.Authorization.Sequence
		if err := cl.Status().Update(ctx, a); err != nil {
			t.Fatalf("unchanged source authorization blocked status update: %v", err)
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
			a := &DNSRegistration{ObjectMeta: metav1.ObjectMeta{GenerateName: "registration-", Namespace: "validation"}, Spec: DNSRegistrationSpec{DNSZoneRef: DNSObjectReference{Name: "zone", UID: "zone-uid"}, Name: "web", RecordTypes: []RRType{RRTypeAAAA}, PublicationPolicy: DNSPublicationPolicyEligibleContributions, TTLSeconds: ttl}}
			requireInvalid(t, cl.Create(ctx, a))
		})
	}
	t.Run("contribution-authority", func(t *testing.T) {
		a := &DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "contribution", Namespace: "validation"}, Spec: DNSRecordContributionSpec{RegistrationRef: DNSObjectReference{Name: "web", UID: "registration-uid", Generation: 1}, GrantRef: DNSObjectReference{Name: "grant", UID: "grant-uid"}, RecordSets: []DNSContributionRecordSet{}}}
		if err := cl.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		a.Spec.GrantRef.Name = "other"
		requireInvalid(t, cl.Update(ctx, a))
	})
	validateServiceDNSContracts(t, cl)
	validateNamingAPISchemas(t, cl)

}
