// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// stagingNow anchors every fixture to one instant, so ages compare exactly
// instead of racing wall time.
var stagingNow = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

func cond(condType, status, reason, message string) metav1.Condition {
	return condAt(condType, status, reason, message, stagingNow)
}

func condAt(condType, status, reason, message string, at time.Time) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionStatus(status),
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.NewTime(at),
	}
}

// epochCond builds a condition carrying the API's own unset-timestamp
// sentinel, the way a freshly created DNSRecordSet's default status does.
func epochCond(condType, status, reason, message string) metav1.Condition {
	c := condAt(condType, status, reason, message, time.Unix(0, 0).UTC())
	return c
}

func zone(domain string, conds ...metav1.Condition) *dnsv1alpha1.DNSZone {
	return zoneCreated(domain, stagingNow.Add(-time.Hour), conds...)
}

func zoneCreated(domain string, created time.Time, conds ...metav1.Condition) *dnsv1alpha1.DNSZone {
	return &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:              domain,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: dnsv1alpha1.DNSZoneSpec{DomainName: domain},
		Status: dnsv1alpha1.DNSZoneStatus{
			Conditions: conds,
		},
	}
}

func recordSet(name string, records []dnsv1alpha1.RecordEntry, statuses ...dnsv1alpha1.RecordSetStatus) *dnsv1alpha1.DNSRecordSet {
	return recordSetCreated(name, stagingNow.Add(-time.Hour), records, statuses...)
}

func recordSetCreated(name string, created time.Time, records []dnsv1alpha1.RecordEntry, statuses ...dnsv1alpha1.RecordSetStatus) *dnsv1alpha1.DNSRecordSet {
	return &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    records,
		},
		Status: dnsv1alpha1.DNSRecordSetStatus{
			RecordSets: statuses,
		},
	}
}

func rejected(name string, conds ...metav1.Condition) *dnsv1alpha1.DNSRecordSet {
	rs := recordSet(name, []dnsv1alpha1.RecordEntry{{Name: name}})
	rs.Status.Conditions = conds
	return rs
}

func ownerStatus(name string, conds ...metav1.Condition) dnsv1alpha1.RecordSetStatus {
	return dnsv1alpha1.RecordSetStatus{Name: name, Conditions: conds}
}
