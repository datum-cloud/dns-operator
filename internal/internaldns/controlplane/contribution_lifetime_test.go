package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

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
