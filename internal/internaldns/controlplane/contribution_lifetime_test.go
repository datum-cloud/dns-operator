package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestPublicationReadinessPreservesCurrentCompilerStatus(t *testing.T) {
	input := fixture(time.Now())
	zone, registration, contribution := input.Zone, input.Registrations[0], input.Contributions[0]
	zone.Namespace, registration.Namespace, contribution.Namespace = "project", "project", "project"
	owner := &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-" + model.OpaqueToken(string(zone.UID))[:20], Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: zone.UID, WriterEpoch: 1, ActiveManifestName: "active"}}
	manifest := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{WriterEpoch: 1, Revision: 2, RegistrationFences: []dnsv1alpha1.DNSRegistrationFence{{UID: registration.UID, Generation: registration.Generation}}, ContributionFences: []dnsv1alpha1.DNSContributionFence{{UID: contribution.UID, Generation: contribution.Generation, Sequence: contribution.Generation}}}, Status: dnsv1alpha1.DNSPublicationManifestStatus{Conditions: []metav1.Condition{{Type: "Published", Status: metav1.ConditionTrue}}}}
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, manifest, &registration, &contribution).WithStatusSubresource(&dnsv1alpha1.DNSRegistration{}, &dnsv1alpha1.DNSRecordContribution{}).Build()
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(&contribution), &contribution); err != nil {
		t.Fatal(err)
	}
	stale := *contribution.DeepCopy()
	contribution.Status.ObservedGeneration = contribution.Generation
	contribution.Status.Conditions = []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue, ObservedGeneration: contribution.Generation, Reason: "Accepted", LastTransitionTime: metav1.Now()}}
	if err := cl.Status().Update(context.Background(), &contribution); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: cl, Options: ReconcilerOptions{PlatformNamespace: "platform"}}
	if err := r.aggregatePublicationStatus(context.Background(), zone, []dnsv1alpha1.DNSRegistration{registration}, []dnsv1alpha1.DNSRecordContribution{stale}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(&contribution), &contribution); err != nil {
		t.Fatal(err)
	}
	accepted, published := apimeta.FindStatusCondition(contribution.Status.Conditions, "Accepted"), apimeta.FindStatusCondition(contribution.Status.Conditions, "Published")
	if accepted == nil || accepted.Status != metav1.ConditionTrue || published == nil || published.Status != metav1.ConditionTrue || contribution.Status.ObservedGeneration != contribution.Generation || contribution.Status.PublishedRevision != 2 {
		t.Fatalf("readiness lost current compiler output: %#v", contribution.Status)
	}
}

func TestContributionLifetimesSurvivePublicationTakeover(t *testing.T) {
	input := fixture(time.Now())
	contribution := input.Contributions[0]
	contribution.Namespace = "project"
	original := PublicationPlan{ZoneUID: input.Zone.UID, ZoneApex: input.Zone.Spec.DomainName, Contributions: []ContributionFence{
		{UID: contribution.UID, Generation: contribution.Generation, Sequence: contribution.Generation, SourceName: contribution.Name},
		{UID: "retired-endpoint", Generation: 1, Sequence: 1, SourceName: "deleted", Retired: true},
	}}
	wire, err := WirePlan(original)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	chunk := &dnsv1alpha1.DNSPublicationChunk{ObjectMeta: metav1.ObjectMeta{Name: "committed-000", Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationChunkSpec{WriterEpoch: 1, Revision: 1, Index: 0, SHA256: model.Hash(payload), Payload: payload}}
	manifest := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "committed", Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{ZoneRef: dnsv1alpha1.DNSObjectReference{UID: input.Zone.UID}, WriterEpoch: 1, Revision: 1, ContentHash: model.Hash(payload), Chunks: []dnsv1alpha1.DNSPublicationChunkReference{{Name: chunk.Name, SHA256: chunk.Spec.SHA256, Size: int32(len(payload))}}}}
	owner := &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: input.Zone.UID, WriterEpoch: 2, ActiveManifestName: manifest.Name}}
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(chunk, manifest, owner, &contribution).Build()
	r := &Reconciler{Client: cl, Options: ReconcilerOptions{PlatformNamespace: "platform"}}
	restored, err := r.loadPrevious(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Contributions) != 2 || restored.Contributions[0].SourceName != contribution.Name || restored.Contributions[1].SourceName != "deleted" || !restored.Contributions[1].Retired {
		t.Fatalf("lifetime metadata lost in committed snapshot: %#v", restored.Contributions)
	}
	confirmed, err := r.confirmContributions(context.Background(), "project", nil, restored)
	if err != nil || len(confirmed) != 1 {
		t.Fatalf("takeover could not confirm live source: %#v, %v", confirmed, err)
	}
	input.Previous, input.Contributions = restored, confirmed
	compiled, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Plan.RRsets) != 1 || len(compiled.Plan.Contributions) != 2 || !compiled.Plan.Contributions[1].Retired {
		t.Fatalf("takeover lost live records or retirement: %#v", compiled.Plan)
	}
}

func TestDeletedContributionCannotReturnThroughReplay(t *testing.T) {
	in := fixture(time.Now())
	live, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	in.Previous = &live.Plan
	in.Contributions = nil
	deleted, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted.Plan.Contributions) != 1 || !deleted.Plan.Contributions[0].Retired {
		t.Fatal("deleted lifetime was not retained")
	}
	for _, generation := range []int64{18, 19} {
		replayed := fixture(time.Now())
		replayed.Previous = &deleted.Plan
		replayed.Contributions[0].Generation = generation
		got, err := Compile(replayed)
		if err != nil {
			t.Fatal(err)
		}
		if got.Reasons["contribution-a"] != "RetiredContribution" || len(got.Plan.RRsets) != 0 {
			t.Fatalf("retired lifetime returned: %#v", got)
		}
	}
	recreated := fixture(time.Now())
	recreated.Previous = &deleted.Plan
	recreated.Contributions[0].UID = "replacement-contribution"
	got, err := Compile(recreated)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Plan.RRsets) != 1 || !got.Accepted["replacement-contribution"] {
		t.Fatalf("replacement lifetime did not publish: %#v", got)
	}
}

func TestContributionAbsenceRequiresAuthoritativeRead(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	contribution := &dnsv1alpha1.DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "endpoint", Namespace: "project", UID: "lifetime", Generation: 1}}
	previous := &PublicationPlan{Contributions: []ContributionFence{{UID: contribution.UID, SourceName: contribution.Name, Generation: 1, Sequence: 1}}}
	cache := fake.NewClientBuilder().WithScheme(scheme).Build()
	authoritative := fake.NewClientBuilder().WithScheme(scheme).WithObjects(contribution).Build()
	r := &Reconciler{Client: cache, APIReader: authoritative}
	confirmed, err := r.confirmContributions(context.Background(), "project", nil, previous)
	if err != nil || len(confirmed) != 1 || confirmed[0].UID != contribution.UID {
		t.Fatalf("cache omission lost live desired state: %#v, %v", confirmed, err)
	}
	r.APIReader = fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return fmt.Errorf("source API unavailable")
	}}).Build()
	if _, err := r.confirmContributions(context.Background(), "project", nil, previous); err == nil {
		t.Fatal("source read error was treated as deletion")
	}
	r.APIReader = cache
	confirmed, err = r.confirmContributions(context.Background(), "project", []dnsv1alpha1.DNSRecordContribution{*contribution}, previous)
	if err != nil || len(confirmed) != 0 {
		t.Fatalf("authoritative deletion retained stale cached source: %#v, %v", confirmed, err)
	}
}
