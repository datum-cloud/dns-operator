// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
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
