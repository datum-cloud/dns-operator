package controller

import (
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

func programmedRecordStatus(name string, status metav1.ConditionStatus, reason, message string) dnsv1alpha1.RecordSetStatus {
	return dnsv1alpha1.RecordSetStatus{
		Name: name,
		Conditions: []metav1.Condition{{
			Type:    CondProgrammed,
			Status:  status,
			Reason:  reason,
			Message: message,
		}},
	}
}

func TestAggregateProgrammedStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		statuses       []dnsv1alpha1.RecordSetStatus
		wantProgrammed bool
		wantReason     string
		wantMessage    string
	}{
		{
			name:           "no record statuses",
			wantProgrammed: true,
		},
		{
			name: "every record programmed",
			statuses: []dnsv1alpha1.RecordSetStatus{
				programmedRecordStatus("www", metav1.ConditionTrue, ReasonProgrammed, "Record successfully applied to PDNS"),
				programmedRecordStatus("api", metav1.ConditionTrue, ReasonProgrammed, "Record successfully applied to PDNS"),
			},
			wantProgrammed: true,
		},
		{
			name: "record without a condition is still converging",
			statuses: []dnsv1alpha1.RecordSetStatus{
				programmedRecordStatus("api", metav1.ConditionTrue, ReasonProgrammed, "Record successfully applied to PDNS"),
				{Name: "www"},
			},
			wantReason:  ReasonPending,
			wantMessage: messageRecordsPending,
		},
		{
			name: "record awaiting removal from spec is still converging",
			statuses: []dnsv1alpha1.RecordSetStatus{
				programmedRecordStatus("www", metav1.ConditionFalse, ReasonPending, "Record no longer present in spec"),
			},
			wantReason:  ReasonPending,
			wantMessage: messageRecordsPending,
		},
		{
			name: "one blocked record surfaces its reason and name",
			statuses: []dnsv1alpha1.RecordSetStatus{
				programmedRecordStatus("api", metav1.ConditionTrue, ReasonProgrammed, "Record successfully applied to PDNS"),
				programmedRecordStatus("www", metav1.ConditionFalse, ReasonNotOwner, "Another DNSRecordSet owns this record"),
			},
			wantReason:  ReasonNotOwner,
			wantMessage: "www: Another DNSRecordSet owns this record",
		},
		{
			name: "a blocked record outranks a converging one",
			statuses: []dnsv1alpha1.RecordSetStatus{
				{Name: "aaa"},
				programmedRecordStatus("www", metav1.ConditionFalse, ReasonConflict, "A conflicting record already exists for this name."),
			},
			wantReason:  ReasonConflict,
			wantMessage: "www: A conflicting record already exists for this name.",
		},
		{
			name: "several blocked records report every cause in record name order",
			statuses: []dnsv1alpha1.RecordSetStatus{
				programmedRecordStatus("www", metav1.ConditionFalse, ReasonNotOwner, "Another DNSRecordSet owns this record"),
				programmedRecordStatus("api", metav1.ConditionFalse, ReasonPDNSError, "The DNS record was rejected as invalid: Bad TTL"),
			},
			wantReason:  ReasonPDNSError,
			wantMessage: "api: The DNS record was rejected as invalid: Bad TTL; www: Another DNSRecordSet owns this record",
		},
		{
			name: "blocked record listing is capped",
			statuses: []dnsv1alpha1.RecordSetStatus{
				programmedRecordStatus("a", metav1.ConditionFalse, ReasonNotOwner, "Another DNSRecordSet owns this record"),
				programmedRecordStatus("b", metav1.ConditionFalse, ReasonNotOwner, "Another DNSRecordSet owns this record"),
				programmedRecordStatus("c", metav1.ConditionFalse, ReasonNotOwner, "Another DNSRecordSet owns this record"),
				programmedRecordStatus("d", metav1.ConditionFalse, ReasonNotOwner, "Another DNSRecordSet owns this record"),
				programmedRecordStatus("e", metav1.ConditionFalse, ReasonNotOwner, "Another DNSRecordSet owns this record"),
			},
			wantReason: ReasonNotOwner,
			wantMessage: "a: Another DNSRecordSet owns this record; b: Another DNSRecordSet owns this record; " +
				"c: Another DNSRecordSet owns this record; and 2 more",
		},
		{
			name: "blocked record without a message falls back to its reason",
			statuses: []dnsv1alpha1.RecordSetStatus{
				programmedRecordStatus("www", metav1.ConditionFalse, ReasonNotOwner, ""),
			},
			wantReason:  ReasonNotOwner,
			wantMessage: "www: NotOwner",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			programmed, reason, message := aggregateProgrammedStatus(tc.statuses)
			if programmed != tc.wantProgrammed {
				t.Fatalf("programmed = %v, want %v", programmed, tc.wantProgrammed)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if message != tc.wantMessage {
				t.Errorf("message = %q, want %q", message, tc.wantMessage)
			}
		})
	}
}

func TestRefreshProgrammedConditionRecovery(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			Records: []dnsv1alpha1.RecordEntry{{Name: "www"}, {Name: "api"}},
		},
		Status: dnsv1alpha1.DNSRecordSetStatus{
			RecordSets: []dnsv1alpha1.RecordSetStatus{
				programmedRecordStatus("www", metav1.ConditionFalse, ReasonNotOwner, "Another DNSRecordSet owns this record"),
				programmedRecordStatus("api", metav1.ConditionTrue, ReasonProgrammed, "Record successfully applied to PDNS"),
			},
		},
	}

	assertAggregate := func(t *testing.T, stage string, status metav1.ConditionStatus, reason, message string) {
		t.Helper()

		cond := apimeta.FindStatusCondition(rs.Status.Conditions, CondProgrammed)
		if cond == nil {
			t.Fatalf("%s: expected aggregate %s condition", stage, CondProgrammed)
		}
		if cond.Status != status || cond.Reason != reason || cond.Message != message {
			t.Fatalf("%s: aggregate condition = %s/%s/%q, want %s/%s/%q",
				stage, cond.Status, cond.Reason, cond.Message, status, reason, message)
		}
	}

	refreshProgrammedCondition(rs)
	assertAggregate(t, "blocked", metav1.ConditionFalse, ReasonNotOwner, "www: Another DNSRecordSet owns this record")

	rs.Status.RecordSets[0] = dnsv1alpha1.RecordSetStatus{Name: "www"}
	refreshProgrammedCondition(rs)
	assertAggregate(t, "converging", metav1.ConditionFalse, ReasonPending, messageRecordsPending)

	rs.Status.RecordSets[0] = programmedRecordStatus("www", metav1.ConditionTrue, ReasonProgrammed, "Record successfully applied to PDNS")
	refreshProgrammedCondition(rs)
	assertAggregate(t, "programmed", metav1.ConditionTrue, ReasonProgrammed, "All records programmed")
}
