// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// cond is a terse condition builder for the table tests below.
func cond(condType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.NewTime(time.Now()),
	}
}

// crdDefaultConditions mirrors what the API server stamps onto a freshly created
// DNSRecordSet, epoch timestamps and all.
func crdDefaultConditions() []metav1.Condition {
	epoch := metav1.NewTime(time.Unix(0, 0))
	return []metav1.Condition{
		{Type: "Accepted", Status: metav1.ConditionUnknown, Reason: "Pending", Message: "Waiting for controller", LastTransitionTime: epoch},
		{Type: "Programmed", Status: metav1.ConditionUnknown, Reason: "Pending", Message: "Waiting for controller", LastTransitionTime: epoch},
	}
}

func TestFindCondition(t *testing.T) {
	conds := []metav1.Condition{
		cond("Accepted", metav1.ConditionTrue, "Accepted", ""),
		cond("Programmed", metav1.ConditionFalse, "Conflict", "boom"),
	}

	if got := FindCondition(conds, "Programmed"); got == nil || got.Reason != "Conflict" {
		t.Errorf("FindCondition(Programmed) = %#v, want the Conflict condition", got)
	}
	if got := FindCondition(conds, "Nonexistent"); got != nil {
		t.Errorf("FindCondition(Nonexistent) = %#v, want nil", got)
	}
	if got := FindCondition(nil, "Programmed"); got != nil {
		t.Errorf("FindCondition(nil) = %#v, want nil", got)
	}
}

func TestZoneStatus(t *testing.T) {
	tests := []struct {
		name       string
		zone       *dnsv1alpha1.DNSZone
		wantWord   string
		wantDetail string
	}{
		{
			name:       "nil zone",
			zone:       nil,
			wantWord:   "Unknown",
			wantDetail: "no zone data",
		},
		{
			name: "programmed with records",
			zone: &dnsv1alpha1.DNSZone{Status: dnsv1alpha1.DNSZoneStatus{
				RecordCount: 12,
				Conditions: []metav1.Condition{
					cond("Accepted", metav1.ConditionTrue, "Accepted", ""),
					cond("Programmed", metav1.ConditionTrue, "Programmed", ""),
				},
			}},
			wantWord:   "OK",
			wantDetail: "zone programmed, 12 records live",
		},
		{
			name: "programmed with exactly one record reads singular",
			zone: &dnsv1alpha1.DNSZone{Status: dnsv1alpha1.DNSZoneStatus{
				RecordCount: 1,
				Conditions:  []metav1.Condition{cond("Programmed", metav1.ConditionTrue, "Programmed", "")},
			}},
			wantWord:   "OK",
			wantDetail: "zone programmed, 1 record live",
		},
		{
			name: "admission rejection outranks programmed",
			zone: &dnsv1alpha1.DNSZone{Status: dnsv1alpha1.DNSZoneStatus{
				Conditions: []metav1.Condition{
					cond("Accepted", metav1.ConditionFalse, "InvalidDNSZone", "The zone class does not exist."),
					cond("Programmed", metav1.ConditionTrue, "Programmed", ""),
				},
			}},
			wantWord:   "Rejected",
			wantDetail: "The zone class does not exist.",
		},
		{
			name: "rejection with no message falls back to the reason",
			zone: &dnsv1alpha1.DNSZone{Status: dnsv1alpha1.DNSZoneStatus{
				Conditions: []metav1.Condition{cond("Accepted", metav1.ConditionFalse, "InvalidDNSZone", "")},
			}},
			wantWord:   "Rejected",
			wantDetail: "InvalidDNSZone",
		},
		{
			name: "pending reason",
			zone: &dnsv1alpha1.DNSZone{Status: dnsv1alpha1.DNSZoneStatus{
				Conditions: []metav1.Condition{cond("Programmed", metav1.ConditionFalse, "Pending", "Waiting for controller")},
			}},
			wantWord:   "Pending",
			wantDetail: "Waiting for controller",
		},
		{
			name: "backend failure",
			zone: &dnsv1alpha1.DNSZone{Status: dnsv1alpha1.DNSZoneStatus{
				Conditions: []metav1.Condition{
					cond("Programmed", metav1.ConditionFalse, "PDNSError", "The DNS backend rejected the zone."),
				},
			}},
			wantWord:   "Error",
			wantDetail: "The DNS backend rejected the zone.",
		},
		{
			name: "an unknown reason is passed through raw",
			zone: &dnsv1alpha1.DNSZone{Status: dnsv1alpha1.DNSZoneStatus{
				Conditions: []metav1.Condition{cond("Programmed", metav1.ConditionFalse, "SomethingNew", "")},
			}},
			wantWord:   "Error",
			wantDetail: "SomethingNew",
		},
		{
			name: "unknown status is pending",
			zone: &dnsv1alpha1.DNSZone{Status: dnsv1alpha1.DNSZoneStatus{
				Conditions: []metav1.Condition{cond("Programmed", metav1.ConditionUnknown, "Pending", "")},
			}},
			wantWord:   "Pending",
			wantDetail: "waiting for the DNS backend",
		},
		{
			name:       "no conditions at all",
			zone:       &dnsv1alpha1.DNSZone{},
			wantWord:   "Pending",
			wantDetail: "waiting for the DNS backend",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			word, detail := ZoneStatus(tc.zone)
			if word != tc.wantWord {
				t.Errorf("word = %q, want %q", word, tc.wantWord)
			}
			if detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", detail, tc.wantDetail)
			}
		})
	}
}

func TestRecordStatus(t *testing.T) {
	// ownerStatusNamed builds a DNSRecordSet with one per-name status entry.
	ownerStatusNamed := func(name string, c metav1.Condition) *dnsv1alpha1.DNSRecordSet {
		return &dnsv1alpha1.DNSRecordSet{Status: dnsv1alpha1.DNSRecordSetStatus{
			Conditions: []metav1.Condition{cond("Accepted", metav1.ConditionTrue, "Accepted", "")},
			RecordSets: []dnsv1alpha1.RecordSetStatus{
				{Name: name, Conditions: []metav1.Condition{c}},
			},
		}}
	}
	// ownerStatus is the common case: the entry is for "www".
	ownerStatus := func(c metav1.Condition) *dnsv1alpha1.DNSRecordSet {
		return ownerStatusNamed("www", c)
	}

	tests := []struct {
		name       string
		rs         *dnsv1alpha1.DNSRecordSet
		owner      string
		wantWord   string
		wantDetail string
	}{
		{
			name:       "nil record set",
			rs:         nil,
			owner:      "www",
			wantWord:   "Unknown",
			wantDetail: "no record set data",
		},
		{
			// Every other branch falls back to a sentence when the server sends
			// no message; the success path used to be the one that returned an
			// empty detail, so a describe view had nothing to print.
			name:       "programmed with no server message still explains itself",
			rs:         ownerStatus(cond("Programmed", metav1.ConditionTrue, "Programmed", "")),
			owner:      "www",
			wantWord:   "Programmed",
			wantDetail: "live in the DNS backend",
		},
		{
			name:       "programmed prefers the server's own message",
			rs:         ownerStatus(cond("Programmed", metav1.ConditionTrue, "Programmed", "Record is live.")),
			owner:      "www",
			wantWord:   "Programmed",
			wantDetail: "Record is live.",
		},
		{
			// The backend keys an RRset by its qualified name, so these are all
			// the same record. Comparing raw strings returned a placid
			// "Pending" for a record that is programmed — and would equally
			// have hidden a Conflict.
			name:       "an uppercase spelling matches",
			rs:         ownerStatus(cond("Programmed", metav1.ConditionTrue, "Programmed", "")),
			owner:      "WWW",
			wantWord:   "Programmed",
			wantDetail: "live in the DNS backend",
		},
		{
			name:       "a trailing dot on the status name matches a bare label",
			rs:         ownerStatusNamed("www.", cond("Programmed", metav1.ConditionFalse, "Conflict", "Name is taken.")),
			owner:      "www.",
			wantWord:   "Conflict",
			wantDetail: "Name is taken.",
		},
		{
			name: "not owner",
			rs: ownerStatus(cond("Programmed", metav1.ConditionFalse, "NotOwner",
				"Another record set owns this name — example-com-a-legacy")),
			owner:      "www",
			wantWord:   "Not owner",
			wantDetail: "Another record set owns this name — example-com-a-legacy",
		},
		{
			name: "conflict shows the backend message verbatim",
			rs: ownerStatus(cond("Programmed", metav1.ConditionFalse, "Conflict",
				"The record name is outside the zone. Check that the name belongs to this DNS zone.")),
			owner:      "www",
			wantWord:   "Conflict",
			wantDetail: "The record name is outside the zone. Check that the name belongs to this DNS zone.",
		},
		{
			name:       "pdns error",
			rs:         ownerStatus(cond("Programmed", metav1.ConditionFalse, "PDNSError", "The DNS backend returned 422.")),
			owner:      "www",
			wantWord:   "Error",
			wantDetail: "The DNS backend returned 422.",
		},
		{
			name:       "explicit pending",
			rs:         ownerStatus(cond("Programmed", metav1.ConditionFalse, "Pending", "")),
			owner:      "www",
			wantWord:   "Pending",
			wantDetail: "waiting for the DNS backend",
		},
		{
			name: "no per-name status at all is pending",
			rs: &dnsv1alpha1.DNSRecordSet{Status: dnsv1alpha1.DNSRecordSetStatus{
				Conditions: crdDefaultConditions(),
			}},
			owner:      "www",
			wantWord:   "Pending",
			wantDetail: "waiting for the DNS backend",
		},
		{
			name:       "a name absent from recordSets is pending",
			rs:         ownerStatus(cond("Programmed", metav1.ConditionTrue, "Programmed", "")),
			owner:      "api",
			wantWord:   "Pending",
			wantDetail: "waiting for the DNS backend",
		},
		{
			name: "rejected at admission outranks the per-name status",
			rs: &dnsv1alpha1.DNSRecordSet{Status: dnsv1alpha1.DNSRecordSetStatus{
				Conditions: []metav1.Condition{
					cond("Accepted", metav1.ConditionFalse, "InvalidDNSRecordSet",
						"records[0].a is set but recordType is CNAME."),
				},
				RecordSets: []dnsv1alpha1.RecordSetStatus{
					{Name: "www", Conditions: []metav1.Condition{cond("Programmed", metav1.ConditionTrue, "Programmed", "")}},
				},
			}},
			owner:      "www",
			wantWord:   "Rejected",
			wantDetail: "records[0].a is set but recordType is CNAME.",
		},
		{
			name:       "an unknown reason passes through raw on both halves",
			rs:         ownerStatus(cond("Programmed", metav1.ConditionFalse, "QuotaExceeded", "Too many records.")),
			owner:      "www",
			wantWord:   "QuotaExceeded",
			wantDetail: "Too many records.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			word, detail := RecordStatus(tc.rs, tc.owner)
			if word != tc.wantWord {
				t.Errorf("word = %q, want %q", word, tc.wantWord)
			}
			if detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", detail, tc.wantDetail)
			}
		})
	}
}

// With the zone supplied, the apex and fully qualified spellings resolve too.
// These are the ones that cannot be normalised without it.
func TestRecordStatusInZone(t *testing.T) {
	programmed := cond("Programmed", metav1.ConditionTrue, "Programmed", "Record is live.")
	conflicted := cond("Programmed", metav1.ConditionFalse, "Conflict", "Name is taken.")

	withOwner := func(name string, c metav1.Condition) *dnsv1alpha1.DNSRecordSet {
		return &dnsv1alpha1.DNSRecordSet{Status: dnsv1alpha1.DNSRecordSetStatus{
			RecordSets: []dnsv1alpha1.RecordSetStatus{{Name: name, Conditions: []metav1.Condition{c}}},
		}}
	}

	tests := []struct {
		name     string
		rs       *dnsv1alpha1.DNSRecordSet
		owner    string
		zone     string
		wantWord string
	}{
		{
			name:     "@ matches a status stored fully qualified",
			rs:       withOwner("example.com.", programmed),
			owner:    "@",
			zone:     "example.com",
			wantWord: StatusProgrammed,
		},
		{
			name:     "a fully qualified owner matches a status stored as @",
			rs:       withOwner("@", programmed),
			owner:    "example.com.",
			zone:     "example.com",
			wantWord: StatusProgrammed,
		},
		{
			name:     "a relative label matches a status stored fully qualified",
			rs:       withOwner("www.example.com.", conflicted),
			owner:    "www",
			zone:     "example.com",
			wantWord: StatusConflict,
		},
		{
			name:     "a fully qualified owner matches a status stored relative",
			rs:       withOwner("www", conflicted),
			owner:    "www.example.com.",
			zone:     "example.com",
			wantWord: StatusConflict,
		},
		{
			name:     "the zone may carry a trailing dot",
			rs:       withOwner("example.com.", programmed),
			owner:    "@",
			zone:     "example.com.",
			wantWord: StatusProgrammed,
		},
		{
			name:     "a genuinely different name still does not match",
			rs:       withOwner("www.example.com.", programmed),
			owner:    "api",
			zone:     "example.com",
			wantWord: StatusPending,
		},
		{
			name:     "a name in another zone does not match",
			rs:       withOwner("www.other.com.", programmed),
			owner:    "www",
			zone:     "example.com",
			wantWord: StatusPending,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			word, _ := RecordStatusInZone(tc.rs, tc.owner, tc.zone)
			if word != tc.wantWord {
				t.Errorf("word = %q, want %q", word, tc.wantWord)
			}
		})
	}
}

// One bucket can hold two spellings of one owner name, which is two entries in
// status.recordSets for one RRset, and they can disagree. Reporting the first
// would let list order decide whether a record looks healthy.
func TestRecordStatusFoldsEverySpelling(t *testing.T) {
	programmed := cond("Programmed", metav1.ConditionTrue, "Programmed", "Record is live.")
	conflicted := cond("Programmed", metav1.ConditionFalse, "Conflict", "Name is taken.")
	notOwner := cond("Programmed", metav1.ConditionFalse, "NotOwner", "Another set owns this.")
	pdnsError := cond("Programmed", metav1.ConditionFalse, "PDNSError", "Backend rejected it.")
	pending := cond("Programmed", metav1.ConditionFalse, "Pending", "")
	novel := cond("Programmed", metav1.ConditionFalse, "QuotaExceeded", "Too many records.")

	withEntries := func(entries ...dnsv1alpha1.RecordSetStatus) *dnsv1alpha1.DNSRecordSet {
		return &dnsv1alpha1.DNSRecordSet{Status: dnsv1alpha1.DNSRecordSetStatus{RecordSets: entries}}
	}
	entry := func(name string, c metav1.Condition) dnsv1alpha1.RecordSetStatus {
		return dnsv1alpha1.RecordSetStatus{Name: name, Conditions: []metav1.Condition{c}}
	}

	tests := []struct {
		name       string
		rs         *dnsv1alpha1.DNSRecordSet
		wantWord   string
		wantDetail string
	}{
		{
			name:       "a conflict behind a programmed entry still surfaces",
			rs:         withEntries(entry("www", programmed), entry("www.example.com.", conflicted)),
			wantWord:   StatusConflict,
			wantDetail: "Name is taken.",
		},
		{
			name:       "order does not matter",
			rs:         withEntries(entry("www.example.com.", conflicted), entry("www", programmed)),
			wantWord:   StatusConflict,
			wantDetail: "Name is taken.",
		},
		{
			name:       "an unknown reason outranks success",
			rs:         withEntries(entry("www", programmed), entry("WWW", novel)),
			wantWord:   "QuotaExceeded",
			wantDetail: "Too many records.",
		},
		{
			name:     "an unknown reason outranks pending",
			rs:       withEntries(entry("www", pending), entry("www.example.com.", novel)),
			wantWord: "QuotaExceeded",
		},
		{
			name:     "a backend error outranks a conflict",
			rs:       withEntries(entry("www", conflicted), entry("www.example.com.", pdnsError)),
			wantWord: StatusError,
		},
		{
			name:     "a conflict outranks not-owner",
			rs:       withEntries(entry("www", notOwner), entry("www.example.com.", conflicted)),
			wantWord: StatusConflict,
		},
		{
			name:     "not-owner outranks pending",
			rs:       withEntries(entry("www", pending), entry("www.example.com.", notOwner)),
			wantWord: StatusNotOwner,
		},
		{
			name:     "pending outranks programmed",
			rs:       withEntries(entry("www", programmed), entry("www.example.com.", pending)),
			wantWord: StatusPending,
		},
		{
			name:       "all programmed stays programmed",
			rs:         withEntries(entry("www", programmed), entry("www.example.com.", programmed)),
			wantWord:   StatusProgrammed,
			wantDetail: "Record is live.",
		},
		{
			name: "a matching entry with no Programmed condition counts as pending",
			rs: withEntries(
				entry("www", programmed),
				dnsv1alpha1.RecordSetStatus{Name: "www.example.com.", Conditions: nil},
			),
			wantWord: StatusPending,
		},
		{
			name:     "entries for a different name are not folded in",
			rs:       withEntries(entry("www", programmed), entry("api", conflicted)),
			wantWord: StatusProgrammed,
		},
	}

	// The zone is supplied because these spellings need it. Without one,
	// "www" is a relative label and "www." is a root-absolute name, and
	// treating them as the same record would be wrong rather than lenient —
	// see TestRecordStatusFoldsWithoutAZone for what folds regardless.
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			word, detail := RecordStatusInZone(tc.rs, "www", "example.com")
			if word != tc.wantWord {
				t.Errorf("word = %q, want %q", word, tc.wantWord)
			}
			if tc.wantDetail != "" && detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", detail, tc.wantDetail)
			}
		})
	}
}

// Case folding needs no zone, so it applies to RecordStatus too. A trailing dot
// deliberately does not fold without one: "www." is a root-absolute name, and a
// bare "www" is a label in some zone we have not been told.
func TestRecordStatusFoldsWithoutAZone(t *testing.T) {
	rs := &dnsv1alpha1.DNSRecordSet{Status: dnsv1alpha1.DNSRecordSetStatus{
		RecordSets: []dnsv1alpha1.RecordSetStatus{
			{Name: "www", Conditions: []metav1.Condition{
				cond("Programmed", metav1.ConditionTrue, "Programmed", ""),
			}},
			{Name: "WWW", Conditions: []metav1.Condition{
				cond("Programmed", metav1.ConditionFalse, "Conflict", "Name is taken."),
			}},
		},
	}}

	if word, _ := RecordStatus(rs, "www"); word != StatusConflict {
		t.Errorf("word = %q, want %q — case is not significant in DNS", word, StatusConflict)
	}

	rooted := &dnsv1alpha1.DNSRecordSet{Status: dnsv1alpha1.DNSRecordSetStatus{
		RecordSets: []dnsv1alpha1.RecordSetStatus{
			{Name: "www", Conditions: []metav1.Condition{
				cond("Programmed", metav1.ConditionTrue, "Programmed", ""),
			}},
			{Name: "www.", Conditions: []metav1.Condition{
				cond("Programmed", metav1.ConditionFalse, "Conflict", "A different name entirely."),
			}},
		},
	}}
	if word, _ := RecordStatus(rooted, "www"); word != StatusProgrammed {
		t.Errorf("word = %q, want %q — a root-absolute name is not the same record", word, StatusProgrammed)
	}
}

// The same fold, across spellings that only resolve with the zone.
func TestRecordStatusInZoneFoldsApexSpellings(t *testing.T) {
	rs := &dnsv1alpha1.DNSRecordSet{Status: dnsv1alpha1.DNSRecordSetStatus{
		RecordSets: []dnsv1alpha1.RecordSetStatus{
			{Name: "@", Conditions: []metav1.Condition{
				cond("Programmed", metav1.ConditionTrue, "Programmed", ""),
			}},
			{Name: "example.com.", Conditions: []metav1.Condition{
				cond("Programmed", metav1.ConditionFalse, "Conflict", "Apex is taken."),
			}},
		},
	}}

	word, detail := RecordStatusInZone(rs, "@", "example.com")
	if word != StatusConflict {
		t.Errorf("word = %q, want %q — the apex has two spellings and one is failing", word, StatusConflict)
	}
	if detail != "Apex is taken." {
		t.Errorf("detail = %q", detail)
	}

	// Without the zone the two spellings cannot be related, so only the literal
	// match is folded. This is the documented limit of RecordStatus.
	if word, _ := RecordStatus(rs, "@"); word != StatusProgrammed {
		t.Errorf("RecordStatus(@) = %q, want %q without a zone to resolve the apex", word, StatusProgrammed)
	}
}

func TestStatusSeverityOrdering(t *testing.T) {
	// An unrecognised reason must sit above both healthy states, so a reason
	// the server grows later is never mistaken for success.
	unknownReason := statusSeverity("SomethingTheServerInvented")

	if unknownReason <= statusSeverity(StatusProgrammed) {
		t.Errorf("an unknown reason (%d) must outrank Programmed (%d)",
			unknownReason, statusSeverity(StatusProgrammed))
	}
	if unknownReason <= statusSeverity(StatusPending) {
		t.Errorf("an unknown reason (%d) must outrank Pending (%d)",
			unknownReason, statusSeverity(StatusPending))
	}
	if unknownReason >= statusSeverity(StatusConflict) {
		t.Errorf("an unknown reason (%d) should rank below a known Conflict (%d)",
			unknownReason, statusSeverity(StatusConflict))
	}

	ordered := []string{
		StatusUnknown, StatusProgrammed, StatusPending,
		StatusNotOwner, StatusConflict, StatusError, StatusRejected,
	}
	for i := 1; i < len(ordered); i++ {
		if statusSeverity(ordered[i]) <= statusSeverity(ordered[i-1]) {
			t.Errorf("%q (%d) must outrank %q (%d)",
				ordered[i], statusSeverity(ordered[i]), ordered[i-1], statusSeverity(ordered[i-1]))
		}
	}
}
