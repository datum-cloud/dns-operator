package controlplane

import (
	"context"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTerminatingInputsWithdrawRecordsAndRetainFences(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		mark func(*CompileInput)
	}{
		{"registration", func(in *CompileInput) {
			in.Registrations[0].Finalizers = []string{"example.test/hold"}
			in.Registrations[0].DeletionTimestamp = &metav1.Time{Time: now.Add(-time.Second)}
		}},
		{"grant", func(in *CompileInput) {
			in.Grants[0].Finalizers = []string{"example.test/hold"}
			in.Grants[0].DeletionTimestamp = &metav1.Time{Time: now.Add(-time.Second)}
		}},
		{"contribution", func(in *CompileInput) {
			in.Contributions[0].Finalizers = []string{"example.test/hold"}
			in.Contributions[0].DeletionTimestamp = &metav1.Time{Time: now.Add(-time.Second)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := fixture(now)
			before, err := Compile(in)
			if err != nil {
				t.Fatal(err)
			}
			in.Previous = &before.Plan
			tc.mark(&in)
			got, err := Compile(in)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Plan.RRsets) != 0 || got.Accepted[in.Contributions[0].UID] {
				t.Fatalf("control retained terminating %s: %#v", tc.name, got)
			}
			if len(got.Plan.Contributions) != 1 || got.Plan.Contributions[0].Sequence != 18 {
				t.Fatalf("lost withdrawal high-water fence: %#v", got.Plan.Contributions)
			}
		})
	}
}

func TestCommittedUntargetedTombstoneCompletes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&dnsv1alpha1.DNSPublicationManifest{}).Build()
	r := &Reconciler{Client: cl, Options: ReconcilerOptions{PlatformNamespace: "platform", ChunkSize: 1024}}
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "orphan", UID: "zone-orphan"}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "orphan.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	manifest, err := r.persistPublication(ctx, zone, "tombstone", 1, 1, 0, true, nil, model.EmptyContentHash(), nil, nil, nil, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	owner := &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: zone.UID, WriterEpoch: 1, ActiveManifestName: manifest.Name}}
	if err := cl.Create(ctx, owner); err != nil {
		t.Fatal(err)
	}
	var outboxes dnsv1alpha1.DNSTransportOutboxList
	if err := cl.List(ctx, &outboxes); err != nil {
		t.Fatal(err)
	}
	if len(outboxes.Items) != 0 {
		t.Fatalf("untargeted tombstone unexpectedly has a delivery path: %d", len(outboxes.Items))
	}
	if !r.tombstoneAcknowledged(ctx, owner) {
		t.Fatal("committed untargeted tombstone did not complete")
	}
	owner.Spec.WriterEpoch++
	if !r.tombstoneAcknowledged(ctx, owner) {
		t.Fatal("takeover lost the committed zero-target withdrawal")
	}
}

func TestTerminatingAuthorityDoesNotIssueOrBindWriterEpochs(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, terminating := range []string{"registration", "grant"} {
		t.Run(terminating, func(t *testing.T) {
			in := fixture(now)
			in.Grants[0].Status = dnsv1alpha1.DNSContributionGrantStatus{}
			stamp := metav1.NewTime(now)
			if terminating == "registration" {
				in.Registrations[0].DeletionTimestamp = &stamp
				in.Registrations[0].Finalizers = []string{"hold.test/finalizer"}
			} else {
				in.Grants[0].DeletionTimestamp = &stamp
				in.Grants[0].Finalizers = []string{"hold.test/finalizer"}
			}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&dnsv1alpha1.DNSContributionGrant{}, &dnsv1alpha1.DNSRecordContribution{}).WithObjects(&in.Registrations[0], &in.Grants[0]).Build()
			r := &Reconciler{Client: cl}
			if _, err := r.issueGrants(context.Background(), in.Grants, in.Registrations, now); err != nil {
				t.Fatal(err)
			}
			if in.Grants[0].Status.ActiveWriterEpoch != 0 {
				t.Fatal("terminating authority received a new epoch")
			}
		})
	}
	in := fixture(now)
	stamp := metav1.NewTime(now)
	in.Contributions[0].DeletionTimestamp = &stamp
	in.Contributions[0].Finalizers = []string{"hold.test/finalizer"}
	in.Contributions[0].Status.WriterEpoch = 0
	in.Grants[0].Status.Conditions = []metav1.Condition{{Type: "Active", Status: metav1.ConditionTrue}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&dnsv1alpha1.DNSRecordContribution{}).WithObjects(&in.Contributions[0]).Build()
	r := &Reconciler{Client: cl}
	if _, err := r.bindContributionEpochs(context.Background(), in.Contributions, in.Grants, now); err != nil {
		t.Fatal(err)
	}
	if in.Contributions[0].Status.WriterEpoch != 0 {
		t.Fatal("terminating contribution received an epoch")
	}
}
