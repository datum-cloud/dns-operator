// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func validateServiceDNSContracts(t *testing.T, cl client.Client) {
	t.Helper()
	ctx := context.Background()
	invalid := func(t *testing.T, err error) {
		t.Helper()
		if !apierrors.IsInvalid(err) {
			t.Fatalf("expected API validation rejection, got %v", err)
		}
	}
	manifest := func(name string) *DNSPublicationManifest {
		return &DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "validation"}, Spec: DNSPublicationManifestSpec{ZoneRef: DNSObjectReference{Name: "zone", UID: "zone-uid"}, ZoneApex: "datum.internal", ContextUIDs: []types.UID{}, WriterEpoch: 1, Revision: 2, PreviousRevision: 1, Chunks: []DNSPublicationChunkReference{{Name: "chunk", SHA256: strings.Repeat("a", 64), Size: 8}}, ContentHash: strings.Repeat("b", 64), GeneratedAt: metav1.Now()}}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*DNSPublicationManifest)
	}{
		{"manifest-zero-epoch", func(a *DNSPublicationManifest) { a.Spec.WriterEpoch = 0 }},
		{"manifest-zero-revision", func(a *DNSPublicationManifest) { a.Spec.Revision = 0 }},
		{"manifest-future-predecessor", func(a *DNSPublicationManifest) { a.Spec.PreviousRevision = a.Spec.Revision }},
		{"manifest-invalid-hash", func(a *DNSPublicationManifest) { a.Spec.ContentHash = "invalid" }},
		{"manifest-empty-publication", func(a *DNSPublicationManifest) { a.Spec.Chunks = []DNSPublicationChunkReference{} }},
		{"manifest-withdrawal-content", func(a *DNSPublicationManifest) { a.Spec.Tombstone = true }},
		{"manifest-empty-chunk", func(a *DNSPublicationManifest) { a.Spec.Chunks[0].Size = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) { a := manifest(tc.name); tc.mutate(a); invalid(t, cl.Create(ctx, a)) })
	}
	t.Run("manifest-status-and-immutability", func(t *testing.T) {
		a := manifest("manifest")
		if err := cl.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		a.Status.ExportState = "Delivered"
		if err := cl.Status().Update(ctx, a); err != nil {
			t.Fatal(err)
		}
		a.Spec.Revision++
		invalid(t, cl.Update(ctx, a))
	})
	t.Run("manifest-withdrawal", func(t *testing.T) {
		a := manifest("withdrawal")
		a.Spec.Tombstone = true
		a.Spec.Chunks = []DNSPublicationChunkReference{}
		a.Spec.ContentHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		if err := cl.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("chunk-empty-payload", func(t *testing.T) {
		a := &DNSPublicationChunk{ObjectMeta: metav1.ObjectMeta{Name: "empty-chunk", Namespace: "validation"}, Spec: DNSPublicationChunkSpec{WriterEpoch: 1, Revision: 1, Index: 0, SHA256: strings.Repeat("a", 64), Payload: []byte{}}}
		invalid(t, cl.Create(ctx, a))
	})
	owner := func(name string) *DNSPublicationOwnership {
		return &DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "validation"}, Spec: DNSPublicationOwnershipSpec{ZoneUID: "zone-uid", HolderIdentity: "worker-a", WriterEpoch: 2, NextRevision: 3, LeaseUntil: metav1.NewTime(time.Now().Add(time.Minute))}}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*DNSPublicationOwnership)
	}{
		{"owner-change-zone", func(a *DNSPublicationOwnership) { a.Spec.ZoneUID = "other" }},
		{"owner-epoch-rollback", func(a *DNSPublicationOwnership) { a.Spec.WriterEpoch-- }},
		{"owner-revision-rollback", func(a *DNSPublicationOwnership) { a.Spec.NextRevision-- }},
		{"owner-unfenced-transfer", func(a *DNSPublicationOwnership) { a.Spec.HolderIdentity = "worker-b" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := owner(tc.name)
			if err := cl.Create(ctx, a); err != nil {
				t.Fatal(err)
			}
			tc.mutate(a)
			invalid(t, cl.Update(ctx, a))
		})
	}
	t.Run("owner-fenced-transfer", func(t *testing.T) {
		a := owner("transfer")
		if err := cl.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		a.Spec.HolderIdentity = "worker-b"
		a.Spec.WriterEpoch++
		if err := cl.Update(ctx, a); err != nil {
			t.Fatal(err)
		}
	})
}
