package controlplane

import (
	"context"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

func TestProjectEventsShareOneQueueKey(t *testing.T) {
	want := types.NamespacedName{Namespace: "project-ns", Name: projectQueueKey}
	for _, obj := range []client.Object{
		&dnsv1alpha1.DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "instance-a", Namespace: "project-ns"}},
		&dnsv1alpha1.DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "instance-b", Namespace: "project-ns"}},
		&dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: "project-ns"}},
	} {
		requests := projectRequest(context.Background(), obj)
		if len(requests) != 1 || requests[0].NamespacedName != want {
			t.Fatalf("%s mapped to %v, want the single project key %v", obj.GetName(), requests, want)
		}
	}
}

func TestContributionStatusWritesDoNotQueueAnotherPass(t *testing.T) {
	old := &dnsv1alpha1.DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "instance-a", Namespace: "project-ns", Generation: 3}}
	statusOnly := old.DeepCopy()
	statusOnly.Status.ObservedGeneration = 3
	specChange := old.DeepCopy()
	specChange.Generation = 4
	p := predicate.GenerationChangedPredicate{}
	if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: statusOnly}) {
		t.Fatal("a status-only contribution update queued another project pass")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: specChange}) {
		t.Fatal("a contribution spec change did not queue a project pass")
	}
	if !p.Create(event.CreateEvent{Object: old}) || !p.Delete(event.DeleteEvent{Object: old}) {
		t.Fatal("contribution creation and deletion must queue a project pass")
	}
}
