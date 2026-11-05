// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestMarkExportedKeepsConcurrentAcknowledgements(t *testing.T) {
	ctx := context.Background()
	m := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "pub-1", Namespace: "platform"}}
	base := testClient(t, m)
	raced := false
	cl := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*dnsv1alpha1.DNSPublicationManifest); !ok || raced {
				return nil
			}
			raced = true
			var fresh dnsv1alpha1.DNSPublicationManifest
			if err := c.Get(ctx, key, &fresh); err != nil {
				return err
			}
			fresh.Status.ReplicaAcknowledgements = []dnsv1alpha1.DNSApplyAcknowledgement{{MemberID: "regional-0", Phase: "Verified"}}
			return c.Status().Update(ctx, &fresh)
		},
	})
	outbox := &Outbox{Client: cl, Namespace: "platform"}

	first := metav1.NewTime(time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC))
	if err := outbox.markExported(ctx, "pub-1", &first); err != nil {
		t.Fatalf("an export racing an acknowledgement must not conflict: %v", err)
	}
	var got dnsv1alpha1.DNSPublicationManifest
	if err := base.Get(ctx, client.ObjectKeyFromObject(m), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.ReplicaAcknowledgements) != 1 {
		t.Fatalf("export dropped a concurrent acknowledgement: %#v", got.Status.ReplicaAcknowledgements)
	}
	if got.Status.ExportedAt == nil || !got.Status.ExportedAt.Equal(&first) || got.Status.ExportState != dnsValueAcknowledged {
		t.Fatalf("first export not recorded: %#v", got.Status)
	}

	version := got.ResourceVersion
	later := metav1.NewTime(first.Add(time.Second))
	if err := outbox.markExported(ctx, "pub-1", &later); err != nil {
		t.Fatal(err)
	}
	if err := base.Get(ctx, client.ObjectKeyFromObject(m), &got); err != nil {
		t.Fatal(err)
	}
	if got.ResourceVersion != version || !got.Status.ExportedAt.Equal(&first) {
		t.Fatalf("a second region's export rewrote the manifest: version %s -> %s, exportedAt %v", version, got.ResourceVersion, got.Status.ExportedAt)
	}

	if err := outbox.markExported(ctx, "gone", &first); err != nil {
		t.Fatalf("a collected manifest is not an export failure: %v", err)
	}
}

func TestFailedExportMarkLeavesTheOutboxPending(t *testing.T) {
	if os.Getenv("INTERNAL_DNS_TEST_PUBLICATION_STORE") != "memory" {
		t.Skip("the fake client cannot merge-patch the outbox's uint64 status")
	}
	ctx := context.Background()
	now := time.Now().UTC()
	manifest := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "manifest", Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{ZoneRef: dnsv1alpha1.DNSObjectReference{UID: "zone"}, ZoneApex: "prod.internal", WriterEpoch: 1, Revision: 2,
		ServingTargets: []dnsv1alpha1.DNSPublicationTarget{{Region: "east", Shard: "shared"}}}}
	owner := &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-" + model.OpaqueToken("zone")[:20], Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: "zone", WriterEpoch: 1, ActiveManifestName: manifest.Name}}
	published := model.PublicationManifest{ManifestUID: manifest.Name, ZoneUID: "zone", Apex: "prod.internal", WriterEpoch: 1, Revision: 2, PreviousRevision: 1, Tombstone: true, Chunks: []model.ChunkRef{}, ContentHash: model.EmptyContentHash(), GeneratedAt: now}
	env, err := model.NewEnvelope(model.KindPublicationManifest, "east:manifest", "east", "shared", "zone", 1, 2, now, published)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(env)
	out := &dnsv1alpha1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: model.PublicationActivationName(manifest.Name, "east", "shared"), Namespace: "platform"}, Spec: dnsv1alpha1.DNSTransportOutboxSpec{Subject: model.RecordManifestSubject("east", "shared", "zone"), ResourceUID: "zone", WriterEpoch: 1, Revision: 2, ManifestRef: dnsv1alpha1.DNSObjectReference{Name: manifest.Name}, Activation: true, Payload: payload, PayloadHash: model.Hash(payload)}}
	base := testClient(t, owner, manifest, out)
	failing := true
	cl := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if _, ok := obj.(*dnsv1alpha1.DNSPublicationManifest); ok && failing {
				return errors.New("store unavailable")
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	worker := Outbox{Client: cl, Namespace: "platform", Publisher: &recordingPublisher{}}
	if err := worker.Step(ctx); err == nil {
		t.Fatal("a failed export mark must be reported")
	}
	var got dnsv1alpha1.DNSTransportOutbox
	if err := base.Get(ctx, client.ObjectKeyFromObject(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.State == dnsValueAcknowledged {
		t.Fatal("the outbox settled although the manifest was never marked exported")
	}

	failing = false
	if err := worker.Step(ctx); err != nil {
		t.Fatal(err)
	}
	var m dnsv1alpha1.DNSPublicationManifest
	if err := base.Get(ctx, client.ObjectKeyFromObject(manifest), &m); err != nil {
		t.Fatal(err)
	}
	if err := base.Get(ctx, client.ObjectKeyFromObject(out), &got); err != nil {
		t.Fatal(err)
	}
	if m.Status.ExportedAt == nil || got.Status.State != dnsValueAcknowledged {
		t.Fatalf("retry did not mark the export: manifest %#v outbox %#v", m.Status, got.Status)
	}
}
