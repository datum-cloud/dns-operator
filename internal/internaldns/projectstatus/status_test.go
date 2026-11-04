// SPDX-License-Identifier: AGPL-3.0-only

package projectstatus

import (
	"reflect"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestProjectionIsQuiescentAfterConvergence(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var conditions []metav1.Condition
	if !Published(&conditions, true, 7, now) || !Programmed(&conditions, true, 7, now) || !Available(&conditions, false, 7, now) {
		t.Fatal("initial projection did not report changes")
	}
	converged := append([]metav1.Condition(nil), conditions...)
	if Published(&conditions, true, 7, now.Add(time.Minute)) || Programmed(&conditions, true, 7, now.Add(time.Minute)) || Available(&conditions, false, 7, now.Add(time.Minute)) {
		t.Fatal("unchanged projection reported a status mutation")
	}
	if !reflect.DeepEqual(conditions, converged) {
		t.Fatalf("unchanged projection churned conditions:\n%#v\n%#v", converged, conditions)
	}
}

func TestRegionalPublishedNamesPendingRegions(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var entries []dnsv1alpha1.DNSRegionPublicationStatus
	SetRegion(&entries, dnsv1alpha1.DNSRegionPublicationStatus{Region: "us-east-1", ObservedGeneration: 3, Published: true})
	SetRegion(&entries, dnsv1alpha1.DNSRegionPublicationStatus{Region: "us-central-1", ObservedGeneration: 2, Published: true})
	if entries[0].Region != "us-central-1" {
		t.Fatalf("entries are not in region order: %#v", entries)
	}
	expected := []string{"us-central-1", "us-east-1"}
	pending := PendingRegions(entries, expected, 3)
	if !reflect.DeepEqual(pending, []string{"us-central-1"}) {
		t.Fatalf("pending = %v, want the region behind the generation", pending)
	}
	var conditions []metav1.Condition
	PublishedInRegions(&conditions, pending, 3, now)
	if conditions[0].Status != metav1.ConditionFalse || conditions[0].Message != "publication awaits required replica verification in us-central-1" {
		t.Fatalf("condition = %#v", conditions[0])
	}
	SetRegion(&entries, dnsv1alpha1.DNSRegionPublicationStatus{Region: "us-central-1", ObservedGeneration: 3, Published: true})
	PublishedInRegions(&conditions, PendingRegions(entries, expected, 3), 3, now)
	if conditions[0].Status != metav1.ConditionTrue {
		t.Fatalf("every region published but condition = %#v", conditions[0])
	}
	if PendingRegions(entries, []string{"us-east-1"}, 3) != nil {
		t.Fatal("a region outside the expected set held publication back")
	}
	RemoveRegion(&entries, "us-east-1")
	if len(entries) != 1 || entries[0].Region != "us-central-1" {
		t.Fatalf("entries after removal = %#v", entries)
	}
}
