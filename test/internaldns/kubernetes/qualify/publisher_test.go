// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"testing"

	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type statusConflictClient struct {
	client.Client
	patches int
}

func (c *statusConflictClient) Patch(ctx context.Context, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patches++
	if c.patches == 1 {
		var current dnsv1.DNSRecordContribution
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(object), &current); err != nil {
			return err
		}
		current.Status.PublishedRevision = 21
		if err := c.Client.Status().Update(ctx, &current); err != nil {
			return err
		}
		return apierrors.NewConflict(schema.GroupResource{Group: dnsv1.GroupVersion.Group, Resource: "dnsrecordcontributions"}, object.GetName(), errors.New("DNS readiness changed"))
	}
	return c.Client.Patch(ctx, object, patch, opts...)
}

func TestContributionSpecRetryPreservesDNSReadiness(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dnsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	contribution := dnsv1.DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: "application"}, Spec: dnsv1.DNSRecordContributionSpec{RecordSets: recordSets("web", "10.42.0.10", ""), Eligible: true}}
	source := &statusConflictClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(&contribution).WithStatusSubresource(&dnsv1.DNSRecordContribution{}).Build()}
	if err := mutateContribution(context.Background(), source, &contribution, func(spec *dnsv1.DNSRecordContributionSpec) {
		spec.RecordSets = recordSets("web", "10.42.0.30", "")
		spec.Eligible = false
		spec.Reason = "EndpointNotReady"
	}); err != nil {
		t.Fatal(err)
	}
	if source.patches != 2 {
		t.Fatalf("patch attempts = %d, want 2", source.patches)
	}
	var stored dnsv1.DNSRecordContribution
	if err := source.Get(context.Background(), client.ObjectKeyFromObject(&contribution), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.PublishedRevision != 21 {
		t.Fatal("producer write changed DNS readiness status")
	}
	if stored.Spec.Eligible || stored.Spec.Reason != "EndpointNotReady" || len(stored.Spec.RecordSets) != 1 || len(stored.Spec.RecordSets[0].Records) != 1 || stored.Spec.RecordSets[0].Records[0].A == nil || stored.Spec.RecordSets[0].Records[0].A.Content != "10.42.0.30" {
		t.Fatal("records and eligibility were not published together")
	}
}
