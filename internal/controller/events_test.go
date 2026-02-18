// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// --------------------------------------------------------------------------
// programmedConditionTransitioned tests
// --------------------------------------------------------------------------

// TestProgrammedConditionTransitioned verifies that
// programmedConditionTransitioned returns true only when the Programmed
// condition status changes between two reconcile observations (e.g. nil -> True,
// True -> False, False -> True), and false when the status is unchanged or when
// the current condition is absent.
func TestProgrammedConditionTransitioned(t *testing.T) {
	t.Parallel()

	condTrue := &metav1.Condition{
		Type:   CondProgrammed,
		Status: metav1.ConditionTrue,
	}
	condFalse := &metav1.Condition{
		Type:   CondProgrammed,
		Status: metav1.ConditionFalse,
	}

	// Each case describes a (prev, curr) pair and whether a transition event
	// should be emitted. The subtest name doubles as documentation of the rule.
	tests := []struct {
		name string
		prev *metav1.Condition
		curr *metav1.Condition
		want bool
	}{
		{
			name: "nil prev, True curr: first-time programming emits event",
			prev: nil,
			curr: condTrue,
			want: true,
		},
		{
			name: "nil prev, False curr: no event on first failure",
			prev: nil,
			curr: condFalse,
			want: false,
		},
		{
			name: "True prev, False curr: programming failure emits event",
			prev: condTrue,
			curr: condFalse,
			want: true,
		},
		{
			name: "False prev, True curr: recovery emits event",
			prev: condFalse,
			curr: condTrue,
			want: true,
		},
		{
			name: "False prev, False curr: no transition, no event",
			prev: condFalse,
			curr: condFalse,
			want: false,
		},
		{
			name: "True prev, True curr: no transition, no event",
			prev: condTrue,
			curr: condTrue,
			want: false,
		},
		{
			name: "True prev, nil curr: condition removed, no event",
			prev: condTrue,
			curr: nil,
			want: false,
		},
		{
			name: "False prev, nil curr: condition removed, no event",
			prev: condFalse,
			curr: nil,
			want: false,
		},
		{
			name: "nil prev, nil curr: both absent, no event",
			prev: nil,
			curr: nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := programmedConditionTransitioned(tt.prev, tt.curr)
			if got != tt.want {
				t.Errorf("programmedConditionTransitioned(%v, %v) = %v, want %v",
					tt.prev, tt.curr, got, tt.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// recordZoneActivityEvent tests
// --------------------------------------------------------------------------

// TestRecordZoneActivityEvent_NilRecorder verifies that recordZoneActivityEvent
// does not panic when called with a nil recorder, so callers that omit the
// recorder (e.g. in unit tests or during startup) are safe.
func TestRecordZoneActivityEvent_NilRecorder(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-zone",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "pdns",
		},
	}

	// Should not panic with nil recorder.
	recordZoneActivityEvent(nil, zone,
		corev1.EventTypeNormal,
		EventReasonZoneProgrammed,
		ActivityTypeZoneProgrammed,
		"DNSZone %q successfully programmed",
		zone.Name,
	)
}

// TestRecordZoneActivityEvent_EmitsWithCorrectAnnotations verifies that
// recordZoneActivityEvent emits exactly one AnnotatedEventf call and that the
// resulting event carries the correct eventType, reason, activity-type
// annotation, domain-name annotation, observed-generation annotation, and does
// NOT include the record-type annotation (which is exclusive to record-set events).
func TestRecordZoneActivityEvent_EmitsWithCorrectAnnotations(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "my-zone",
			Namespace:  "default",
			Generation: 3,
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "pdns",
		},
	}

	// Each case exercises a different event reason/activity-type combination for
	// a DNSZone. The assertions after the call check both the Kubernetes event
	// fields (eventType, reason) and the activity annotations attached to it.
	tests := []struct {
		name           string
		eventType      string
		reason         string
		activityType   string
		messageFmt     string
		wantReason     string
		wantEventType  string
		wantActvType   string
		wantDomainName string
		wantGeneration string
	}{
		{
			name:           "ZoneProgrammed normal event",
			eventType:      corev1.EventTypeNormal,
			reason:         EventReasonZoneProgrammed,
			activityType:   ActivityTypeZoneProgrammed,
			messageFmt:     "DNSZone %q successfully programmed",
			wantReason:     EventReasonZoneProgrammed,
			wantEventType:  corev1.EventTypeNormal,
			wantActvType:   ActivityTypeZoneProgrammed,
			wantDomainName: "example.com",
			wantGeneration: "3",
		},
		{
			name:           "ZoneProgrammingFailed warning event",
			eventType:      corev1.EventTypeWarning,
			reason:         EventReasonZoneProgrammingFailed,
			activityType:   ActivityTypeZoneProgrammingFailed,
			messageFmt:     "DNSZone %q programming failed",
			wantReason:     EventReasonZoneProgrammingFailed,
			wantEventType:  corev1.EventTypeWarning,
			wantActvType:   ActivityTypeZoneProgrammingFailed,
			wantDomainName: "example.com",
			wantGeneration: "3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fakeRecorder := record.NewFakeRecorder(10)
			// Capture annotations via a wrapping recorder that records annotated calls.
			annotatedRecorder := &annotationCapturingRecorder{FakeRecorder: fakeRecorder}

			recordZoneActivityEvent(annotatedRecorder, zone,
				tt.eventType,
				tt.reason,
				tt.activityType,
				tt.messageFmt,
				zone.Name,
			)

			if len(annotatedRecorder.calls) != 1 {
				t.Fatalf("expected 1 annotated event call, got %d", len(annotatedRecorder.calls))
			}
			call := annotatedRecorder.calls[0]

			if call.annotations[AnnotationEventType] != tt.wantActvType {
				t.Errorf("annotation %q = %q, want %q", AnnotationEventType, call.annotations[AnnotationEventType], tt.wantActvType)
			}
			if call.annotations[AnnotationObservedGeneration] != tt.wantGeneration {
				t.Errorf("annotation %q = %q, want %q", AnnotationObservedGeneration, call.annotations[AnnotationObservedGeneration], tt.wantGeneration)
			}
			if call.annotations[AnnotationDomainName] != tt.wantDomainName {
				t.Errorf("annotation %q = %q, want %q", AnnotationDomainName, call.annotations[AnnotationDomainName], tt.wantDomainName)
			}
			if _, hasRecordType := call.annotations[AnnotationRecordType]; hasRecordType {
				t.Errorf("zone event should not have %q annotation", AnnotationRecordType)
			}
			if call.eventType != tt.wantEventType {
				t.Errorf("eventType = %q, want %q", call.eventType, tt.wantEventType)
			}
			if call.reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", call.reason, tt.wantReason)
			}
		})
	}
}

// TestRecordZoneActivityEvent_GenerationInAnnotation verifies that the
// observed-generation annotation is populated from the DNSZone's
// ObjectMeta.Generation field, confirming the annotation tracks spec
// revisions correctly across updates.
func TestRecordZoneActivityEvent_GenerationInAnnotation(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "gen-zone",
			Namespace:  "default",
			Generation: 42,
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "gen.example.com",
			DNSZoneClassName: "pdns",
		},
	}

	fakeRecorder := record.NewFakeRecorder(10)
	annotatedRecorder := &annotationCapturingRecorder{FakeRecorder: fakeRecorder}

	recordZoneActivityEvent(annotatedRecorder, zone,
		corev1.EventTypeNormal,
		EventReasonZoneProgrammed,
		ActivityTypeZoneProgrammed,
		"DNSZone %q successfully programmed",
		zone.Name,
	)

	if len(annotatedRecorder.calls) != 1 {
		t.Fatalf("expected 1 event, got %d", len(annotatedRecorder.calls))
	}
	wantGen := strconv.FormatInt(42, 10)
	if got := annotatedRecorder.calls[0].annotations[AnnotationObservedGeneration]; got != wantGen {
		t.Errorf("observed-generation annotation = %q, want %q", got, wantGen)
	}
}

// --------------------------------------------------------------------------
// recordRecordSetActivityEvent tests
// --------------------------------------------------------------------------

// TestRecordRecordSetActivityEvent_NilRecorder verifies that
// recordRecordSetActivityEvent does not panic when called with a nil recorder,
// matching the same nil-safety guarantee provided for zone events.
func TestRecordRecordSetActivityEvent_NilRecorder(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-rs",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "test-zone"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}},
			},
		},
	}

	// Should not panic with nil recorder.
	recordRecordSetActivityEvent(nil, rs,
		corev1.EventTypeNormal,
		EventReasonRecordSetProgrammed,
		ActivityTypeRecordSetProgrammed,
		"DNSRecordSet %q successfully programmed to DNS provider",
		rs.Name,
	)
}

// TestRecordRecordSetActivityEvent_EmitsWithCorrectAnnotations verifies that
// recordRecordSetActivityEvent emits exactly one AnnotatedEventf call carrying
// the correct eventType, reason, activity-type annotation, record-type
// annotation, and observed-generation annotation, and does NOT include the
// domain-name annotation (which is exclusive to zone events).
func TestRecordRecordSetActivityEvent_EmitsWithCorrectAnnotations(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "www-records",
			Namespace:  "default",
			Generation: 5,
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "my-zone"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}},
			},
		},
	}

	// Each case exercises a different event reason/activity-type combination for
	// a DNSRecordSet. Assertions confirm both the Kubernetes event fields and the
	// activity annotations, including the record-type annotation that zone events omit.
	tests := []struct {
		name           string
		eventType      string
		reason         string
		activityType   string
		messageFmt     string
		wantReason     string
		wantEventType  string
		wantActvType   string
		wantRecordType string
		wantGeneration string
	}{
		{
			name:           "RecordSetProgrammed normal event",
			eventType:      corev1.EventTypeNormal,
			reason:         EventReasonRecordSetProgrammed,
			activityType:   ActivityTypeRecordSetProgrammed,
			messageFmt:     "DNSRecordSet %q programmed",
			wantReason:     EventReasonRecordSetProgrammed,
			wantEventType:  corev1.EventTypeNormal,
			wantActvType:   ActivityTypeRecordSetProgrammed,
			wantRecordType: "A",
			wantGeneration: "5",
		},
		{
			name:           "RecordSetProgrammingFailed warning event",
			eventType:      corev1.EventTypeWarning,
			reason:         EventReasonRecordSetProgrammingFailed,
			activityType:   ActivityTypeRecordSetProgrammingFailed,
			messageFmt:     "DNSRecordSet %q programming failed",
			wantReason:     EventReasonRecordSetProgrammingFailed,
			wantEventType:  corev1.EventTypeWarning,
			wantActvType:   ActivityTypeRecordSetProgrammingFailed,
			wantRecordType: "A",
			wantGeneration: "5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fakeRecorder := record.NewFakeRecorder(10)
			annotatedRecorder := &annotationCapturingRecorder{FakeRecorder: fakeRecorder}

			recordRecordSetActivityEvent(annotatedRecorder, rs,
				tt.eventType,
				tt.reason,
				tt.activityType,
				tt.messageFmt,
				rs.Name,
			)

			if len(annotatedRecorder.calls) != 1 {
				t.Fatalf("expected 1 annotated event call, got %d", len(annotatedRecorder.calls))
			}
			call := annotatedRecorder.calls[0]

			if call.annotations[AnnotationEventType] != tt.wantActvType {
				t.Errorf("annotation %q = %q, want %q", AnnotationEventType, call.annotations[AnnotationEventType], tt.wantActvType)
			}
			if call.annotations[AnnotationObservedGeneration] != tt.wantGeneration {
				t.Errorf("annotation %q = %q, want %q", AnnotationObservedGeneration, call.annotations[AnnotationObservedGeneration], tt.wantGeneration)
			}
			if call.annotations[AnnotationRecordType] != tt.wantRecordType {
				t.Errorf("annotation %q = %q, want %q", AnnotationRecordType, call.annotations[AnnotationRecordType], tt.wantRecordType)
			}
			if _, hasDomainName := call.annotations[AnnotationDomainName]; hasDomainName {
				t.Errorf("recordset event should not have %q annotation", AnnotationDomainName)
			}
			if call.eventType != tt.wantEventType {
				t.Errorf("eventType = %q, want %q", call.eventType, tt.wantEventType)
			}
			if call.reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", call.reason, tt.wantReason)
			}
		})
	}
}

// TestRecordRecordSetActivityEvent_RecordTypePropagated verifies that the
// record-type annotation reflects the DNSRecordSet's Spec.RecordType for every
// supported RRType value, ensuring the annotation is not hardcoded and correctly
// follows whatever type the caller specifies.
func TestRecordRecordSetActivityEvent_RecordTypePropagated(t *testing.T) {
	t.Parallel()

	recordTypes := []dnsv1alpha1.RRType{
		dnsv1alpha1.RRTypeA,
		dnsv1alpha1.RRTypeAAAA,
		dnsv1alpha1.RRTypeCNAME,
		dnsv1alpha1.RRTypeTXT,
		dnsv1alpha1.RRTypeNS,
	}

	for _, rt := range recordTypes {
		t.Run(string(rt), func(t *testing.T) {
			t.Parallel()

			rs := &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-rs",
					Namespace:  "default",
					Generation: 1,
				},
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					DNSZoneRef: corev1.LocalObjectReference{Name: "test-zone"},
					RecordType: rt,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "@"},
					},
				},
			}

			fakeRecorder := record.NewFakeRecorder(10)
			annotatedRecorder := &annotationCapturingRecorder{FakeRecorder: fakeRecorder}

			recordRecordSetActivityEvent(annotatedRecorder, rs,
				corev1.EventTypeNormal,
				EventReasonRecordSetProgrammed,
				ActivityTypeRecordSetProgrammed,
				"DNSRecordSet %q successfully programmed to DNS provider",
				rs.Name,
			)

			if len(annotatedRecorder.calls) != 1 {
				t.Fatalf("expected 1 event, got %d", len(annotatedRecorder.calls))
			}
			got := annotatedRecorder.calls[0].annotations[AnnotationRecordType]
			if got != string(rt) {
				t.Errorf("record-type annotation = %q, want %q", got, string(rt))
			}
		})
	}
}

// --------------------------------------------------------------------------
// recordZoneActivityEventWithData tests
// --------------------------------------------------------------------------

// TestRecordZoneActivityEventWithData_ResourceIdentityAnnotations verifies that the
// resource-name and resource-namespace annotations are set from the zone object
// and that zone-class is set when DNSZoneClassName is non-empty.
func TestRecordZoneActivityEventWithData_ResourceIdentityAnnotations(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "my-zone",
			Namespace:  "project-ns",
			Generation: 2,
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "pdns",
		},
	}

	fakeRecorder := record.NewFakeRecorder(10)
	ar := &annotationCapturingRecorder{FakeRecorder: fakeRecorder}

	recordZoneActivityEventWithData(ar, ZoneEventData{Zone: zone},
		corev1.EventTypeNormal,
		EventReasonZoneProgrammed,
		ActivityTypeZoneProgrammed,
		"DNSZone %q successfully programmed",
		zone.Name,
	)

	if len(ar.calls) != 1 {
		t.Fatalf("expected 1 event, got %d", len(ar.calls))
	}
	ann := ar.calls[0].annotations

	if got := ann[AnnotationResourceName]; got != "my-zone" {
		t.Errorf("%s = %q, want %q", AnnotationResourceName, got, "my-zone")
	}
	if got := ann[AnnotationResourceNamespace]; got != "project-ns" {
		t.Errorf("%s = %q, want %q", AnnotationResourceNamespace, got, "project-ns")
	}
	if got := ann[AnnotationZoneClass]; got != "pdns" {
		t.Errorf("%s = %q, want %q", AnnotationZoneClass, got, "pdns")
	}
	if _, ok := ann[AnnotationNameservers]; ok {
		t.Errorf("%s should be absent when no nameservers are supplied", AnnotationNameservers)
	}
	if _, ok := ann[AnnotationFailureReason]; ok {
		t.Errorf("%s should be absent when no fail reason is supplied", AnnotationFailureReason)
	}
}

// TestRecordZoneActivityEventWithData_ZoneClassAbsentWhenEmpty verifies that the
// zone-class annotation is omitted when DNSZoneClassName is not set.
func TestRecordZoneActivityEventWithData_ZoneClassAbsentWhenEmpty(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: "ns", Generation: 1},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.com", DNSZoneClassName: ""},
	}

	fakeRecorder := record.NewFakeRecorder(10)
	ar := &annotationCapturingRecorder{FakeRecorder: fakeRecorder}

	recordZoneActivityEventWithData(ar, ZoneEventData{Zone: zone},
		corev1.EventTypeNormal, EventReasonZoneProgrammed, ActivityTypeZoneProgrammed, "msg")

	if len(ar.calls) != 1 {
		t.Fatalf("expected 1 event, got %d", len(ar.calls))
	}
	if _, ok := ar.calls[0].annotations[AnnotationZoneClass]; ok {
		t.Errorf("%s should be absent when DNSZoneClassName is empty", AnnotationZoneClass)
	}
}

// TestRecordZoneActivityEventWithData_NameserversAnnotation verifies that the
// nameservers annotation is set as a comma-separated list when provided.
func TestRecordZoneActivityEventWithData_NameserversAnnotation(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: "ns", Generation: 1},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.com", DNSZoneClassName: "pdns"},
	}

	fakeRecorder := record.NewFakeRecorder(10)
	ar := &annotationCapturingRecorder{FakeRecorder: fakeRecorder}

	recordZoneActivityEventWithData(ar,
		ZoneEventData{
			Zone:        zone,
			Nameservers: []string{"ns1.example.com", "ns2.example.com"},
		},
		corev1.EventTypeNormal, EventReasonZoneProgrammed, ActivityTypeZoneProgrammed,
		"programmed",
	)

	if len(ar.calls) != 1 {
		t.Fatalf("expected 1 event, got %d", len(ar.calls))
	}
	got := ar.calls[0].annotations[AnnotationNameservers]
	want := "ns1.example.com,ns2.example.com"
	if got != want {
		t.Errorf("%s = %q, want %q", AnnotationNameservers, got, want)
	}
}

// TestRecordZoneActivityEventWithData_FailReasonAnnotation verifies that the
// failure-reason annotation is set when FailReason is non-empty.
func TestRecordZoneActivityEventWithData_FailReasonAnnotation(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: "ns", Generation: 1},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.com", DNSZoneClassName: "pdns"},
	}

	fakeRecorder := record.NewFakeRecorder(10)
	ar := &annotationCapturingRecorder{FakeRecorder: fakeRecorder}

	recordZoneActivityEventWithData(ar,
		ZoneEventData{
			Zone:       zone,
			FailReason: "upstream provider rejected the zone",
		},
		corev1.EventTypeWarning,
		EventReasonZoneProgrammingFailed,
		ActivityTypeZoneProgrammingFailed,
		"programming failed",
	)

	if len(ar.calls) != 1 {
		t.Fatalf("expected 1 event, got %d", len(ar.calls))
	}
	got := ar.calls[0].annotations[AnnotationFailureReason]
	want := "upstream provider rejected the zone"
	if got != want {
		t.Errorf("%s = %q, want %q", AnnotationFailureReason, got, want)
	}
}

// --------------------------------------------------------------------------
// recordRecordSetActivityEventWithData tests
// --------------------------------------------------------------------------

// TestRecordRecordSetActivityEventWithData_ResourceIdentityAnnotations verifies
// that resource-name, resource-namespace, and zone-ref annotations are always
// present, that record-count and record-name are set when records exist, and
// that domain-name is present only when DomainName is supplied.
func TestRecordRecordSetActivityEventWithData_ResourceIdentityAnnotations(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "www-records",
			Namespace:  "project-ns",
			Generation: 3,
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "my-zone"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}},
				{Name: "api", A: &dnsv1alpha1.ARecordSpec{Content: "5.6.7.8"}},
			},
		},
	}

	fakeRecorder := record.NewFakeRecorder(10)
	ar := &annotationCapturingRecorder{FakeRecorder: fakeRecorder}

	recordRecordSetActivityEventWithData(ar,
		RecordSetEventData{
			RecordSet:  rs,
			DomainName: "example.com",
		},
		corev1.EventTypeNormal,
		EventReasonRecordSetProgrammed,
		ActivityTypeRecordSetProgrammed,
		"programmed",
	)

	if len(ar.calls) != 1 {
		t.Fatalf("expected 1 event, got %d", len(ar.calls))
	}
	ann := ar.calls[0].annotations

	if got := ann[AnnotationResourceName]; got != "www-records" {
		t.Errorf("%s = %q, want %q", AnnotationResourceName, got, "www-records")
	}
	if got := ann[AnnotationResourceNamespace]; got != "project-ns" {
		t.Errorf("%s = %q, want %q", AnnotationResourceNamespace, got, "project-ns")
	}
	if got := ann[AnnotationZoneRef]; got != "my-zone" {
		t.Errorf("%s = %q, want %q", AnnotationZoneRef, got, "my-zone")
	}
	if got := ann[AnnotationDomainName]; got != "example.com" {
		t.Errorf("%s = %q, want %q", AnnotationDomainName, got, "example.com")
	}
	if got := ann[AnnotationRecordCount]; got != "2" {
		t.Errorf("%s = %q, want %q", AnnotationRecordCount, got, "2")
	}
	if got := ann[AnnotationRecordNames]; got != "www,api" {
		t.Errorf("%s = %q, want %q", AnnotationRecordNames, got, "www,api")
	}
}

// TestRecordRecordSetActivityEventWithData_DomainNameAbsentWhenEmpty verifies
// that the domain-name annotation is omitted when DomainName is empty.
func TestRecordRecordSetActivityEventWithData_DomainNameAbsentWhenEmpty(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns", Generation: 1},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "1.1.1.1"}}},
		},
	}

	fakeRecorder := record.NewFakeRecorder(10)
	ar := &annotationCapturingRecorder{FakeRecorder: fakeRecorder}

	recordRecordSetActivityEventWithData(ar,
		RecordSetEventData{RecordSet: rs},
		corev1.EventTypeNormal, EventReasonRecordSetProgrammed, ActivityTypeRecordSetProgrammed, "programmed",
	)

	if len(ar.calls) != 1 {
		t.Fatalf("expected 1 event, got %d", len(ar.calls))
	}
	if _, ok := ar.calls[0].annotations[AnnotationDomainName]; ok {
		t.Errorf("%s should be absent when DomainName is empty", AnnotationDomainName)
	}
}

// TestRecordRecordSetActivityEventWithData_IPAddressesForARecords verifies that
// the ip-addresses annotation is set to a comma-separated list of A record IPs.
func TestRecordRecordSetActivityEventWithData_IPAddressesForARecords(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns", Generation: 1},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "10.0.0.1"}},
				{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "10.0.0.2"}},
			},
		},
	}

	fakeRecorder := record.NewFakeRecorder(10)
	ar := &annotationCapturingRecorder{FakeRecorder: fakeRecorder}

	recordRecordSetActivityEventWithData(ar, RecordSetEventData{RecordSet: rs},
		corev1.EventTypeNormal, EventReasonRecordSetProgrammed, ActivityTypeRecordSetProgrammed, "programmed",
	)

	if len(ar.calls) != 1 {
		t.Fatalf("expected 1 event, got %d", len(ar.calls))
	}
	got := ar.calls[0].annotations[AnnotationIPAddresses]
	want := "10.0.0.1,10.0.0.2"
	if got != want {
		t.Errorf("%s = %q, want %q", AnnotationIPAddresses, got, want)
	}
}

// TestRecordRecordSetActivityEventWithData_IPAddressesForAAAARecords verifies
// that the ip-addresses annotation is set correctly for AAAA record types.
func TestRecordRecordSetActivityEventWithData_IPAddressesForAAAARecords(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns", Generation: 1},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone"},
			RecordType: dnsv1alpha1.RRTypeAAAA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "@", AAAA: &dnsv1alpha1.AAAARecordSpec{Content: "2001:db8::1"}},
				{Name: "@", AAAA: &dnsv1alpha1.AAAARecordSpec{Content: "2001:db8::2"}},
			},
		},
	}

	fakeRecorder := record.NewFakeRecorder(10)
	ar := &annotationCapturingRecorder{FakeRecorder: fakeRecorder}

	recordRecordSetActivityEventWithData(ar, RecordSetEventData{RecordSet: rs},
		corev1.EventTypeNormal, EventReasonRecordSetProgrammed, ActivityTypeRecordSetProgrammed, "programmed",
	)

	if len(ar.calls) != 1 {
		t.Fatalf("expected 1 event, got %d", len(ar.calls))
	}
	got := ar.calls[0].annotations[AnnotationIPAddresses]
	want := "2001:db8::1,2001:db8::2"
	if got != want {
		t.Errorf("%s = %q, want %q", AnnotationIPAddresses, got, want)
	}
}

// TestRecordRecordSetActivityEventWithData_IPAddressesAbsentForNonIPRecords
// verifies that the ip-addresses annotation is omitted for record types that
// are not A or AAAA.
func TestRecordRecordSetActivityEventWithData_IPAddressesAbsentForNonIPRecords(t *testing.T) {
	t.Parallel()

	nonIPTypes := []dnsv1alpha1.RRType{
		dnsv1alpha1.RRTypeCNAME,
		dnsv1alpha1.RRTypeTXT,
		dnsv1alpha1.RRTypeNS,
		dnsv1alpha1.RRTypeMX,
	}

	for _, rt := range nonIPTypes {
		t.Run(string(rt), func(t *testing.T) {
			t.Parallel()

			rs := &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns", Generation: 1},
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					DNSZoneRef: corev1.LocalObjectReference{Name: "zone"},
					RecordType: rt,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "@"}},
				},
			}

			fakeRecorder := record.NewFakeRecorder(10)
			ar := &annotationCapturingRecorder{FakeRecorder: fakeRecorder}

			recordRecordSetActivityEventWithData(ar, RecordSetEventData{RecordSet: rs},
				corev1.EventTypeNormal, EventReasonRecordSetProgrammed, ActivityTypeRecordSetProgrammed, "programmed",
			)

			if len(ar.calls) != 1 {
				t.Fatalf("expected 1 event, got %d", len(ar.calls))
			}
			if _, ok := ar.calls[0].annotations[AnnotationIPAddresses]; ok {
				t.Errorf("%s should be absent for %s record type", AnnotationIPAddresses, rt)
			}
		})
	}
}

// TestRecordRecordSetActivityEventWithData_FailReasonAnnotation verifies that
// the failure-reason annotation is set when FailReason is non-empty.
func TestRecordRecordSetActivityEventWithData_FailReasonAnnotation(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns", Generation: 1},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "1.1.1.1"}}},
		},
	}

	fakeRecorder := record.NewFakeRecorder(10)
	ar := &annotationCapturingRecorder{FakeRecorder: fakeRecorder}

	recordRecordSetActivityEventWithData(ar,
		RecordSetEventData{
			RecordSet:  rs,
			FailReason: "provider API returned 503",
		},
		corev1.EventTypeWarning,
		EventReasonRecordSetProgrammingFailed,
		ActivityTypeRecordSetProgrammingFailed,
		"programming failed",
	)

	if len(ar.calls) != 1 {
		t.Fatalf("expected 1 event, got %d", len(ar.calls))
	}
	got := ar.calls[0].annotations[AnnotationFailureReason]
	want := "provider API returned 503"
	if got != want {
		t.Errorf("%s = %q, want %q", AnnotationFailureReason, got, want)
	}
}

// TestExtractIPAddresses verifies the extractIPAddresses helper for all covered
// record types, including both A and AAAA, and for non-IP types.
func TestExtractIPAddresses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rs      *dnsv1alpha1.DNSRecordSet
		wantIPs []string
	}{
		{
			name: "A records: extracts content values",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeA,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}},
						{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "5.6.7.8"}},
					},
				},
			},
			wantIPs: []string{"1.2.3.4", "5.6.7.8"},
		},
		{
			name: "AAAA records: extracts content values",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeAAAA,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "@", AAAA: &dnsv1alpha1.AAAARecordSpec{Content: "::1"}},
					},
				},
			},
			wantIPs: []string{"::1"},
		},
		{
			name: "A record with nil A field: skipped",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeA,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "@", A: nil}},
				},
			},
			wantIPs: nil,
		},
		{
			name: "CNAME: returns nil",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeCNAME,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "@", CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "target.example.com"}}},
				},
			},
			wantIPs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractIPAddresses(tt.rs)
			if len(got) != len(tt.wantIPs) {
				t.Fatalf("extractIPAddresses() = %v, want %v", got, tt.wantIPs)
			}
			for i := range got {
				if got[i] != tt.wantIPs[i] {
					t.Errorf("extractIPAddresses()[%d] = %q, want %q", i, got[i], tt.wantIPs[i])
				}
			}
		})
	}
}

// --------------------------------------------------------------------------
// test helpers
// --------------------------------------------------------------------------

// annotatedCall records a single invocation of AnnotatedEventf so tests can
// assert on the annotation map and event fields without inspecting a raw string
// channel.
type annotatedCall struct {
	annotations map[string]string
	eventType   string
	reason      string
	messageFmt  string
}

// annotationCapturingRecorder wraps record.FakeRecorder and additionally
// captures the annotations passed to AnnotatedEventf so tests can assert on them.
type annotationCapturingRecorder struct {
	*record.FakeRecorder
	calls []annotatedCall
}

func (r *annotationCapturingRecorder) AnnotatedEventf(
	object runtime.Object,
	annotations map[string]string,
	eventType, reason, messageFmt string,
	args ...interface{},
) {
	// Copy annotations so the caller cannot mutate our captured copy.
	copied := make(map[string]string, len(annotations))
	for k, v := range annotations {
		copied[k] = v
	}
	r.calls = append(r.calls, annotatedCall{
		annotations: copied,
		eventType:   eventType,
		reason:      reason,
		messageFmt:  messageFmt,
	})
	// Also delegate to FakeRecorder so the Events channel receives something.
	r.FakeRecorder.AnnotatedEventf(object, annotations, eventType, reason, messageFmt, args...)
}
