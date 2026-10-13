// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func validateNamingAPISchemas(t *testing.T, cl client.Client) {
	t.Helper()
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		selector *DNSRegistrationSelector
		allowed  bool
	}{
		{"omitted-selector", nil, true},
		{"empty-selector", &DNSRegistrationSelector{}, true},
		{"in-selector", &DNSRegistrationSelector{MatchExpressions: []DNSRegistrationSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"web"}}}}, true},
		{"exists-selector", &DNSRegistrationSelector{MatchExpressions: []DNSRegistrationSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpExists}}}, true},
		{"invalid-operator", &DNSRegistrationSelector{MatchExpressions: []DNSRegistrationSelectorRequirement{{Key: "app", Operator: "Unsupported"}}}, false},
		{"missing-values", &DNSRegistrationSelector{MatchExpressions: []DNSRegistrationSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpIn}}}, false},
		{"unexpected-values", &DNSRegistrationSelector{MatchExpressions: []DNSRegistrationSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpExists, Values: []string{"web"}}}}, false},
		{"invalid-value", &DNSRegistrationSelector{MatchExpressions: []DNSRegistrationSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"invalid value"}}}}, false},
		{"long-value", &DNSRegistrationSelector{MatchExpressions: []DNSRegistrationSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{strings.Repeat("x", 64)}}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &DNSNamingPolicy{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: "validation"}, Spec: DNSNamingPolicySpec{ResolverContextRef: DNSNameReference{Name: "context"}, AdditionalNames: []DNSAdditionalNameRule{{DNSZoneRef: DNSNameReference{Name: "zone"}, Selector: tc.selector, NamePrefix: "hosts"}}}}
			err := cl.Create(ctx, p)
			if tc.allowed {
				if err != nil {
					t.Fatal(err)
				}
			} else if !apierrors.IsInvalid(err) {
				t.Fatalf("expected API validation rejection, got %v", err)
			}
		})
	}
	t.Run("selector-expression-limit", func(t *testing.T) {
		p := &DNSNamingPolicy{ObjectMeta: metav1.ObjectMeta{Name: "expression-limit", Namespace: "validation"}, Spec: DNSNamingPolicySpec{ResolverContextRef: DNSNameReference{Name: "context"}, AdditionalNames: []DNSAdditionalNameRule{{DNSZoneRef: DNSNameReference{Name: "zone"}, NamePrefix: "hosts", Selector: &DNSRegistrationSelector{}}}}}
		for range 64 {
			p.Spec.AdditionalNames[0].Selector.MatchExpressions = append(p.Spec.AdditionalNames[0].Selector.MatchExpressions, DNSRegistrationSelectorRequirement{Key: "app", Operator: metav1.LabelSelectorOpExists})
		}
		if err := cl.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		p.Spec.AdditionalNames[0].Selector.MatchExpressions = append(p.Spec.AdditionalNames[0].Selector.MatchExpressions, DNSRegistrationSelectorRequirement{Key: "app", Operator: metav1.LabelSelectorOpExists})
		if err := cl.Update(ctx, p); !apierrors.IsInvalid(err) {
			t.Fatalf("expected expression limit rejection, got %v", err)
		}
	})
	for _, tc := range []struct {
		name    string
		context DNSNameReference
		allowed bool
	}{
		{"one-context", DNSNameReference{Name: "context"}, true},
		{"no-consumer", DNSNameReference{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: "validation"}, Spec: DNSZoneAssociationSpec{DNSZoneRef: DNSNameReference{Name: "zone"}, ResolverContextRef: tc.context}}
			err := cl.Create(ctx, a)
			if tc.allowed {
				if err != nil {
					t.Fatal(err)
				}
			} else if !apierrors.IsInvalid(err) {
				t.Fatalf("expected API validation rejection, got %v", err)
			}
		})
	}
}
