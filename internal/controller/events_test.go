// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

const (
	testZoneName  = "my-zone"
	testNamespace = "default"
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
// emitZoneEvent tests
// --------------------------------------------------------------------------

// TestEmitZoneEvent_NilClient verifies that emitZoneEvent does not panic when
// called with a nil EventClient, so callers that omit the client (e.g. in unit
// tests or during startup) are safe.
func TestEmitZoneEvent_NilClient(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-zone",
			Namespace:  testNamespace,
			Generation: 1,
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "pdns",
		},
	}

	// Should not panic with nil client.
	emitZoneEvent(context.Background(), nil, ZoneEventData{Zone: zone},
		corev1.EventTypeNormal,
		EventReasonZoneProgrammed,
		ActivityTypeZoneProgrammed,
		"DNSZone %q successfully programmed",
		zone.Name,
	)
}

// TestEmitZoneEvent_EmitsWithCorrectAnnotations verifies that emitZoneEvent
// creates exactly one event and that the resulting event carries the correct
// eventType, reason, activity-type annotation, domain-name annotation,
// observed-generation annotation, and does NOT include the record-type
// annotation (which is exclusive to record-set events).
func TestEmitZoneEvent_EmitsWithCorrectAnnotations(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testZoneName,
			Namespace:  testNamespace,
			Generation: 3,
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "pdns",
		},
	}

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

			fc := &fakeEventClient{}

			emitZoneEvent(context.Background(), fc, ZoneEventData{Zone: zone},
				tt.eventType,
				tt.reason,
				tt.activityType,
				tt.messageFmt,
				zone.Name,
			)

			if len(fc.events) != 1 {
				t.Fatalf("expected 1 event, got %d", len(fc.events))
			}
			evt := fc.events[0]

			if evt.Annotations[AnnotationEventType] != tt.wantActvType {
				t.Errorf("annotation %q = %q, want %q", AnnotationEventType, evt.Annotations[AnnotationEventType], tt.wantActvType)
			}
			if evt.Annotations[AnnotationObservedGeneration] != tt.wantGeneration {
				t.Errorf("annotation %q = %q, want %q", AnnotationObservedGeneration, evt.Annotations[AnnotationObservedGeneration], tt.wantGeneration)
			}
			if evt.Annotations[AnnotationDomainName] != tt.wantDomainName {
				t.Errorf("annotation %q = %q, want %q", AnnotationDomainName, evt.Annotations[AnnotationDomainName], tt.wantDomainName)
			}
			if _, hasRecordType := evt.Annotations[AnnotationRecordType]; hasRecordType {
				t.Errorf("zone event should not have %q annotation", AnnotationRecordType)
			}
			if evt.Type != tt.wantEventType {
				t.Errorf("Type = %q, want %q", evt.Type, tt.wantEventType)
			}
			if evt.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", evt.Reason, tt.wantReason)
			}
		})
	}
}

// TestEmitZoneEvent_GenerationInAnnotation verifies that the
// observed-generation annotation is populated from the DNSZone's
// ObjectMeta.Generation field.
func TestEmitZoneEvent_GenerationInAnnotation(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "gen-zone",
			Namespace:  testNamespace,
			Generation: 42,
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "gen.example.com",
			DNSZoneClassName: "pdns",
		},
	}

	fc := &fakeEventClient{}

	emitZoneEvent(context.Background(), fc, ZoneEventData{Zone: zone},
		corev1.EventTypeNormal,
		EventReasonZoneProgrammed,
		ActivityTypeZoneProgrammed,
		"DNSZone %q successfully programmed",
		zone.Name,
	)

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	wantGen := strconv.FormatInt(42, 10)
	if got := fc.events[0].Annotations[AnnotationObservedGeneration]; got != wantGen {
		t.Errorf("observed-generation annotation = %q, want %q", got, wantGen)
	}
}

// TestEmitZoneEvent_ResourceIdentityAnnotations verifies that the
// resource-name and resource-namespace annotations are set from the zone
// object and that zone-class is set when DNSZoneClassName is non-empty.
func TestEmitZoneEvent_ResourceIdentityAnnotations(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testZoneName,
			Namespace:  "project-ns",
			Generation: 2,
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "pdns",
		},
	}

	fc := &fakeEventClient{}

	emitZoneEvent(context.Background(), fc, ZoneEventData{Zone: zone},
		corev1.EventTypeNormal,
		EventReasonZoneProgrammed,
		ActivityTypeZoneProgrammed,
		"DNSZone %q successfully programmed",
		zone.Name,
	)

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	ann := fc.events[0].Annotations

	if got := ann[AnnotationResourceName]; got != testZoneName {
		t.Errorf("%s = %q, want %q", AnnotationResourceName, got, testZoneName)
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

// TestEmitZoneEvent_ZoneClassAbsentWhenEmpty verifies that the zone-class
// annotation is omitted when DNSZoneClassName is not set.
func TestEmitZoneEvent_ZoneClassAbsentWhenEmpty(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: "ns", Generation: 1},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.com", DNSZoneClassName: ""},
	}

	fc := &fakeEventClient{}

	emitZoneEvent(context.Background(), fc, ZoneEventData{Zone: zone},
		corev1.EventTypeNormal, EventReasonZoneProgrammed, ActivityTypeZoneProgrammed, "msg")

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	if _, ok := fc.events[0].Annotations[AnnotationZoneClass]; ok {
		t.Errorf("%s should be absent when DNSZoneClassName is empty", AnnotationZoneClass)
	}
}

// TestEmitZoneEvent_NameserversAnnotation verifies that the nameservers
// annotation is set as a comma-separated list when provided.
func TestEmitZoneEvent_NameserversAnnotation(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: "ns", Generation: 1},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.com", DNSZoneClassName: "pdns"},
	}

	fc := &fakeEventClient{}

	emitZoneEvent(context.Background(), fc,
		ZoneEventData{
			Zone:        zone,
			Nameservers: []string{"ns1.example.com", "ns2.example.com"},
		},
		corev1.EventTypeNormal, EventReasonZoneProgrammed, ActivityTypeZoneProgrammed,
		"programmed",
	)

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	got := fc.events[0].Annotations[AnnotationNameservers]
	want := "ns1.example.com,ns2.example.com"
	if got != want {
		t.Errorf("%s = %q, want %q", AnnotationNameservers, got, want)
	}
}

// TestEmitZoneEvent_FailReasonAnnotation verifies that the failure-reason
// annotation is set when FailReason is non-empty.
func TestEmitZoneEvent_FailReasonAnnotation(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: "ns", Generation: 1},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.com", DNSZoneClassName: "pdns"},
	}

	fc := &fakeEventClient{}

	emitZoneEvent(context.Background(), fc,
		ZoneEventData{
			Zone:       zone,
			FailReason: "upstream provider rejected the zone",
		},
		corev1.EventTypeWarning,
		EventReasonZoneProgrammingFailed,
		ActivityTypeZoneProgrammingFailed,
		"programming failed",
	)

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	got := fc.events[0].Annotations[AnnotationFailureReason]
	want := "upstream provider rejected the zone"
	if got != want {
		t.Errorf("%s = %q, want %q", AnnotationFailureReason, got, want)
	}
}

// TestEmitZoneEvent_RegardingIsZone verifies that event.regarding is set to the
// DNSZone ObjectReference.
func TestEmitZoneEvent_RegardingIsZone(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testZoneName,
			Namespace: testNamespace,
			UID:       types.UID("zone-uid-123"),
		},
		Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "example.com", DNSZoneClassName: "pdns"},
	}

	fc := &fakeEventClient{}

	emitZoneEvent(context.Background(), fc, ZoneEventData{Zone: zone},
		corev1.EventTypeNormal, EventReasonZoneProgrammed, ActivityTypeZoneProgrammed, "programmed")

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	ref := fc.events[0].Regarding
	if ref.Kind != "DNSZone" {
		t.Errorf("Regarding.Kind = %q, want %q", ref.Kind, "DNSZone")
	}
	if ref.Name != testZoneName {
		t.Errorf("Regarding.Name = %q, want %q", ref.Name, testZoneName)
	}
	if ref.Namespace != testNamespace {
		t.Errorf("Regarding.Namespace = %q, want %q", ref.Namespace, testNamespace)
	}
	if ref.UID != "zone-uid-123" {
		t.Errorf("Regarding.UID = %q, want %q", ref.UID, "zone-uid-123")
	}
}

// TestEmitZoneEvent_RelatedIsAlwaysNil verifies that event.related is nil for
// zone events (zones already identify themselves via Regarding).
func TestEmitZoneEvent_RelatedIsAlwaysNil(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "z", Namespace: "ns", Generation: 1},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.com", DNSZoneClassName: "pdns"},
	}

	fc := &fakeEventClient{}

	emitZoneEvent(context.Background(), fc, ZoneEventData{Zone: zone},
		corev1.EventTypeNormal, EventReasonZoneProgrammed, ActivityTypeZoneProgrammed, "programmed")

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	if fc.events[0].Related != nil {
		t.Errorf("Related = %v, want nil for zone events", fc.events[0].Related)
	}
}

// --------------------------------------------------------------------------
// emitRecordSetEvent tests
// --------------------------------------------------------------------------

// TestEmitRecordSetEvent_NilClient verifies that emitRecordSetEvent does not
// panic when called with a nil EventClient.
func TestEmitRecordSetEvent_NilClient(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-rs",
			Namespace:  testNamespace,
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

	// Should not panic with nil client.
	emitRecordSetEvent(context.Background(), nil, RecordSetEventData{RecordSet: rs},
		corev1.EventTypeNormal,
		EventReasonRecordSetProgrammed,
		ActivityTypeRecordSetProgrammed,
		"DNSRecordSet %q successfully programmed to DNS provider",
		rs.Name,
	)
}

// TestEmitRecordSetEvent_EmitsWithCorrectAnnotations verifies that
// emitRecordSetEvent creates exactly one event carrying the correct eventType,
// reason, activity-type annotation, record-type annotation, and
// observed-generation annotation, and does NOT include the domain-name
// annotation when not provided.
func TestEmitRecordSetEvent_EmitsWithCorrectAnnotations(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "www-records",
			Namespace:  testNamespace,
			Generation: 5,
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: testZoneName},
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}},
			},
		},
	}

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

			fc := &fakeEventClient{}

			emitRecordSetEvent(context.Background(), fc, RecordSetEventData{RecordSet: rs},
				tt.eventType,
				tt.reason,
				tt.activityType,
				tt.messageFmt,
				rs.Name,
			)

			if len(fc.events) != 1 {
				t.Fatalf("expected 1 event, got %d", len(fc.events))
			}
			evt := fc.events[0]

			if evt.Annotations[AnnotationEventType] != tt.wantActvType {
				t.Errorf("annotation %q = %q, want %q", AnnotationEventType, evt.Annotations[AnnotationEventType], tt.wantActvType)
			}
			if evt.Annotations[AnnotationObservedGeneration] != tt.wantGeneration {
				t.Errorf("annotation %q = %q, want %q", AnnotationObservedGeneration, evt.Annotations[AnnotationObservedGeneration], tt.wantGeneration)
			}
			if evt.Annotations[AnnotationRecordType] != tt.wantRecordType {
				t.Errorf("annotation %q = %q, want %q", AnnotationRecordType, evt.Annotations[AnnotationRecordType], tt.wantRecordType)
			}
			if _, hasDomainName := evt.Annotations[AnnotationDomainName]; hasDomainName {
				t.Errorf("recordset event should not have %q annotation when no domain name provided", AnnotationDomainName)
			}
			if evt.Type != tt.wantEventType {
				t.Errorf("Type = %q, want %q", evt.Type, tt.wantEventType)
			}
			if evt.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", evt.Reason, tt.wantReason)
			}
		})
	}
}

// TestEmitRecordSetEvent_RecordTypePropagated verifies that the record-type
// annotation reflects the DNSRecordSet's Spec.RecordType for every supported
// RRType value.
func TestEmitRecordSetEvent_RecordTypePropagated(t *testing.T) {
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
					Namespace:  testNamespace,
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

			fc := &fakeEventClient{}

			emitRecordSetEvent(context.Background(), fc, RecordSetEventData{RecordSet: rs},
				corev1.EventTypeNormal,
				EventReasonRecordSetProgrammed,
				ActivityTypeRecordSetProgrammed,
				"DNSRecordSet %q successfully programmed to DNS provider",
				rs.Name,
			)

			if len(fc.events) != 1 {
				t.Fatalf("expected 1 event, got %d", len(fc.events))
			}
			got := fc.events[0].Annotations[AnnotationRecordType]
			if got != string(rt) {
				t.Errorf("record-type annotation = %q, want %q", got, string(rt))
			}
		})
	}
}

// TestEmitRecordSetEvent_ResourceIdentityAnnotations verifies that
// resource-name, resource-namespace, and zone-ref annotations are always
// present, that record-count and record-names are set when records exist, and
// that domain-name is present only when DomainName is supplied.
func TestEmitRecordSetEvent_ResourceIdentityAnnotations(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "www-records",
			Namespace:  "project-ns",
			Generation: 3,
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: testZoneName},
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}},
				{Name: "api", A: &dnsv1alpha1.ARecordSpec{Content: "5.6.7.8"}},
			},
		},
	}

	fc := &fakeEventClient{}

	emitRecordSetEvent(context.Background(), fc,
		RecordSetEventData{
			RecordSet:  rs,
			DomainName: "example.com",
		},
		corev1.EventTypeNormal,
		EventReasonRecordSetProgrammed,
		ActivityTypeRecordSetProgrammed,
		"programmed",
	)

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	ann := fc.events[0].Annotations

	if got := ann[AnnotationResourceName]; got != "www-records" {
		t.Errorf("%s = %q, want %q", AnnotationResourceName, got, "www-records")
	}
	if got := ann[AnnotationResourceNamespace]; got != "project-ns" {
		t.Errorf("%s = %q, want %q", AnnotationResourceNamespace, got, "project-ns")
	}
	if got := ann[AnnotationZoneRef]; got != testZoneName {
		t.Errorf("%s = %q, want %q", AnnotationZoneRef, got, testZoneName)
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

// TestEmitRecordSetEvent_DomainNameAbsentWhenEmpty verifies that the
// domain-name annotation is omitted when DomainName is empty and Zone is nil.
func TestEmitRecordSetEvent_DomainNameAbsentWhenEmpty(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns", Generation: 1},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "1.1.1.1"}}},
		},
	}

	fc := &fakeEventClient{}

	emitRecordSetEvent(context.Background(), fc,
		RecordSetEventData{RecordSet: rs},
		corev1.EventTypeNormal, EventReasonRecordSetProgrammed, ActivityTypeRecordSetProgrammed, "programmed",
	)

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	if _, ok := fc.events[0].Annotations[AnnotationDomainName]; ok {
		t.Errorf("%s should be absent when DomainName is empty and Zone is nil", AnnotationDomainName)
	}
}

// TestEmitRecordSetEvent_IPAddressesForARecords verifies that the ip-addresses
// annotation is set to a comma-separated list of A record IPs.
func TestEmitRecordSetEvent_IPAddressesForARecords(t *testing.T) {
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

	fc := &fakeEventClient{}

	emitRecordSetEvent(context.Background(), fc, RecordSetEventData{RecordSet: rs},
		corev1.EventTypeNormal, EventReasonRecordSetProgrammed, ActivityTypeRecordSetProgrammed, "programmed",
	)

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	got := fc.events[0].Annotations[AnnotationIPAddresses]
	want := "10.0.0.1,10.0.0.2"
	if got != want {
		t.Errorf("%s = %q, want %q", AnnotationIPAddresses, got, want)
	}
}

// TestEmitRecordSetEvent_IPAddressesForAAAARecords verifies that the
// ip-addresses annotation is set correctly for AAAA record types.
func TestEmitRecordSetEvent_IPAddressesForAAAARecords(t *testing.T) {
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

	fc := &fakeEventClient{}

	emitRecordSetEvent(context.Background(), fc, RecordSetEventData{RecordSet: rs},
		corev1.EventTypeNormal, EventReasonRecordSetProgrammed, ActivityTypeRecordSetProgrammed, "programmed",
	)

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	got := fc.events[0].Annotations[AnnotationIPAddresses]
	want := "2001:db8::1,2001:db8::2"
	if got != want {
		t.Errorf("%s = %q, want %q", AnnotationIPAddresses, got, want)
	}
}

// TestEmitRecordSetEvent_IPAddressesAbsentForNonIPRecords verifies that the
// ip-addresses annotation is omitted for record types that are not A or AAAA.
func TestEmitRecordSetEvent_IPAddressesAbsentForNonIPRecords(t *testing.T) {
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

			fc := &fakeEventClient{}

			emitRecordSetEvent(context.Background(), fc, RecordSetEventData{RecordSet: rs},
				corev1.EventTypeNormal, EventReasonRecordSetProgrammed, ActivityTypeRecordSetProgrammed, "programmed",
			)

			if len(fc.events) != 1 {
				t.Fatalf("expected 1 event, got %d", len(fc.events))
			}
			if _, ok := fc.events[0].Annotations[AnnotationIPAddresses]; ok {
				t.Errorf("%s should be absent for %s record type", AnnotationIPAddresses, rt)
			}
		})
	}
}

// TestEmitRecordSetEvent_FailReasonAnnotation verifies that the failure-reason
// annotation is set when FailReason is non-empty.
func TestEmitRecordSetEvent_FailReasonAnnotation(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns", Generation: 1},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "1.1.1.1"}}},
		},
	}

	fc := &fakeEventClient{}

	emitRecordSetEvent(context.Background(), fc,
		RecordSetEventData{
			RecordSet:  rs,
			FailReason: "provider API returned 503",
		},
		corev1.EventTypeWarning,
		EventReasonRecordSetProgrammingFailed,
		ActivityTypeRecordSetProgrammingFailed,
		"programming failed",
	)

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	got := fc.events[0].Annotations[AnnotationFailureReason]
	want := "provider API returned 503"
	if got != want {
		t.Errorf("%s = %q, want %q", AnnotationFailureReason, got, want)
	}
}

// TestEmitRecordSetEvent_RegardingIsRecordSet verifies that event.regarding is
// set to the DNSRecordSet ObjectReference.
func TestEmitRecordSetEvent_RegardingIsRecordSet(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "www-records",
			Namespace: testNamespace,
			UID:       types.UID("rs-uid-456"),
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: testZoneName},
			RecordType: dnsv1alpha1.RRTypeA,
		},
	}

	fc := &fakeEventClient{}

	emitRecordSetEvent(context.Background(), fc, RecordSetEventData{RecordSet: rs},
		corev1.EventTypeNormal, EventReasonRecordSetProgrammed, ActivityTypeRecordSetProgrammed,
		"DNSRecordSet %q programmed", rs.Name)

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	ref := fc.events[0].Regarding
	if ref.Kind != "DNSRecordSet" {
		t.Errorf("Regarding.Kind = %q, want %q", ref.Kind, "DNSRecordSet")
	}
	if ref.Name != "www-records" {
		t.Errorf("Regarding.Name = %q, want %q", ref.Name, "www-records")
	}
	if ref.Namespace != testNamespace {
		t.Errorf("Regarding.Namespace = %q, want %q", ref.Namespace, testNamespace)
	}
	if ref.UID != "rs-uid-456" {
		t.Errorf("Regarding.UID = %q, want %q", ref.UID, "rs-uid-456")
	}
}

// TestEmitRecordSetEvent_RelatedIsZone verifies that event.related is set to
// the parent DNSZone ObjectReference when Zone is provided.
func TestEmitRecordSetEvent_RelatedIsZone(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "www-records", Namespace: testNamespace},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: testZoneName},
			RecordType: dnsv1alpha1.RRTypeA,
		},
	}
	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testZoneName,
			Namespace: testNamespace,
			UID:       types.UID("zone-uid-789"),
		},
		Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "example.com", DNSZoneClassName: "pdns"},
	}

	fc := &fakeEventClient{}

	emitRecordSetEvent(context.Background(), fc,
		RecordSetEventData{RecordSet: rs, Zone: zone},
		corev1.EventTypeNormal, EventReasonRecordSetProgrammed, ActivityTypeRecordSetProgrammed,
		"DNSRecordSet %q programmed", rs.Name)

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	related := fc.events[0].Related
	if related == nil {
		t.Fatal("Related = nil, want non-nil DNSZone reference")
	}
	if related.Kind != "DNSZone" {
		t.Errorf("Related.Kind = %q, want %q", related.Kind, "DNSZone")
	}
	if related.Name != testZoneName {
		t.Errorf("Related.Name = %q, want %q", related.Name, testZoneName)
	}
	if related.Namespace != testNamespace {
		t.Errorf("Related.Namespace = %q, want %q", related.Namespace, testNamespace)
	}
	if related.UID != "zone-uid-789" {
		t.Errorf("Related.UID = %q, want %q", related.UID, "zone-uid-789")
	}
}

// TestEmitRecordSetEvent_RelatedNilWhenZoneIsNil verifies that event.related is
// nil when Zone is nil in RecordSetEventData.
func TestEmitRecordSetEvent_RelatedNilWhenZoneIsNil(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns", Generation: 1},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone"},
			RecordType: dnsv1alpha1.RRTypeA,
		},
	}

	fc := &fakeEventClient{}

	emitRecordSetEvent(context.Background(), fc,
		RecordSetEventData{RecordSet: rs, Zone: nil},
		corev1.EventTypeNormal, EventReasonRecordSetProgrammed, ActivityTypeRecordSetProgrammed, "programmed")

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	if fc.events[0].Related != nil {
		t.Errorf("Related = %v, want nil when Zone is nil", fc.events[0].Related)
	}
}

// TestEmitRecordSetEvent_CreateErrorIsSwallowed verifies that Create errors are
// swallowed (no panic, no error return).
func TestEmitRecordSetEvent_CreateErrorIsSwallowed(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns", Generation: 1},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone"},
			RecordType: dnsv1alpha1.RRTypeA,
		},
	}

	fc := &fakeEventClient{err: errFakeCreate}

	// Should not panic or propagate the error.
	emitRecordSetEvent(context.Background(), fc,
		RecordSetEventData{RecordSet: rs},
		corev1.EventTypeNormal, EventReasonRecordSetProgrammed, ActivityTypeRecordSetProgrammed, "programmed")
	// No assertion needed — absence of panic is the assertion.
}

// TestEmitRecordSetEvent_EventTimePopulated verifies that EventTime is
// populated (non-zero).
func TestEmitRecordSetEvent_EventTimePopulated(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns", Generation: 1},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone"},
			RecordType: dnsv1alpha1.RRTypeA,
		},
	}

	fc := &fakeEventClient{}

	emitRecordSetEvent(context.Background(), fc,
		RecordSetEventData{RecordSet: rs},
		corev1.EventTypeNormal, EventReasonRecordSetProgrammed, ActivityTypeRecordSetProgrammed, "programmed")

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	if fc.events[0].EventTime.IsZero() {
		t.Error("EventTime is zero, want non-zero")
	}
}

// TestEmitRecordSetEvent_ReportingControllerSet verifies that
// ReportingController is set to the expected value.
func TestEmitRecordSetEvent_ReportingControllerSet(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns", Generation: 1},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone"},
			RecordType: dnsv1alpha1.RRTypeA,
		},
	}

	fc := &fakeEventClient{}

	emitRecordSetEvent(context.Background(), fc,
		RecordSetEventData{RecordSet: rs},
		corev1.EventTypeNormal, EventReasonRecordSetProgrammed, ActivityTypeRecordSetProgrammed, "programmed")

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	want := "dns.networking.miloapis.com/dns-operator"
	if got := fc.events[0].ReportingController; got != want {
		t.Errorf("ReportingController = %q, want %q", got, want)
	}
}

// TestEmitRecordSetEvent_DomainNameFromZone verifies that DomainName is derived
// from Zone.Spec.DomainName when the DomainName field is empty.
func TestEmitRecordSetEvent_DomainNameFromZone(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns", Generation: 1},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: testZoneName},
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}}},
		},
	}
	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: testZoneName, Namespace: "ns"},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "zone-derived.example.com", DNSZoneClassName: "pdns"},
	}

	fc := &fakeEventClient{}

	// DomainName is empty; should be derived from Zone.Spec.DomainName.
	emitRecordSetEvent(context.Background(), fc,
		RecordSetEventData{RecordSet: rs, Zone: zone, DomainName: ""},
		corev1.EventTypeNormal, EventReasonRecordSetProgrammed, ActivityTypeRecordSetProgrammed, "programmed")

	if len(fc.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(fc.events))
	}
	got := fc.events[0].Annotations[AnnotationDomainName]
	want := "zone-derived.example.com"
	if got != want {
		t.Errorf("%s = %q, want %q", AnnotationDomainName, got, want)
	}
}

// --------------------------------------------------------------------------
// extractIPAddresses tests (pure helper — no recorder dependency)
// --------------------------------------------------------------------------

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
// Display annotation helper tests
// --------------------------------------------------------------------------

// TestExtractCNAMETarget verifies extraction of CNAME targets.
func TestExtractCNAMETarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rs   *dnsv1alpha1.DNSRecordSet
		want string
	}{
		{
			name: "CNAME record returns target",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeCNAME,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "api", CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "api.internal.example.com"}},
					},
				},
			},
			want: "api.internal.example.com",
		},
		{
			name: "A record returns empty",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeA,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}},
					},
				},
			},
			want: "",
		},
		{
			name: "CNAME with nil spec returns empty",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeCNAME,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "api", CNAME: nil}},
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractCNAMETarget(tt.rs)
			if got != tt.want {
				t.Errorf("extractCNAMETarget() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestExtractMXHosts verifies extraction of MX hosts with preferences.
func TestExtractMXHosts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rs   *dnsv1alpha1.DNSRecordSet
		want string
	}{
		{
			name: "single MX record",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeMX,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "@", MX: &dnsv1alpha1.MXRecordSpec{Preference: 10, Exchange: "mail.example.com"}},
					},
				},
			},
			want: "10 mail.example.com",
		},
		{
			name: "multiple MX records",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeMX,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "@", MX: &dnsv1alpha1.MXRecordSpec{Preference: 10, Exchange: "mail.example.com"}},
						{Name: "@", MX: &dnsv1alpha1.MXRecordSpec{Preference: 20, Exchange: "mail2.example.com"}},
					},
				},
			},
			want: "10 mail.example.com, 20 mail2.example.com",
		},
		{
			name: "non-MX record returns empty",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeA,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}}},
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractMXHosts(tt.rs)
			if got != tt.want {
				t.Errorf("extractMXHosts() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBuildFQDN verifies FQDN construction from record name and zone domain.
func TestBuildFQDN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		recordName string
		zoneDomain string
		want       string
	}{
		{
			name:       "subdomain",
			recordName: "www",
			zoneDomain: "example.com",
			want:       "www.example.com",
		},
		{
			name:       "apex record",
			recordName: "@",
			zoneDomain: "example.com",
			want:       "example.com",
		},
		{
			name:       "nested subdomain",
			recordName: "api.v2",
			zoneDomain: "example.com",
			want:       "api.v2.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildFQDN(tt.recordName, tt.zoneDomain)
			if got != tt.want {
				t.Errorf("buildFQDN(%q, %q) = %q, want %q", tt.recordName, tt.zoneDomain, got, tt.want)
			}
		})
	}
}

// TestComputeDisplayName verifies FQDN computation for display annotations.
func TestComputeDisplayName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		rs         *dnsv1alpha1.DNSRecordSet
		zoneDomain string
		want       string
	}{
		{
			name: "single record",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					Records: []dnsv1alpha1.RecordEntry{{Name: "www"}},
				},
			},
			zoneDomain: "example.com",
			want:       "www.example.com",
		},
		{
			name: "multiple records same name",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "www"},
						{Name: "www"},
					},
				},
			},
			zoneDomain: "example.com",
			want:       "www.example.com",
		},
		{
			name: "multiple records different names",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "www"},
						{Name: "api"},
					},
				},
			},
			zoneDomain: "example.com",
			want:       "www.example.com, api.example.com",
		},
		{
			name: "apex record",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					Records: []dnsv1alpha1.RecordEntry{{Name: "@"}},
				},
			},
			zoneDomain: "example.com",
			want:       "example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := computeDisplayName(tt.rs, tt.zoneDomain)
			if got != tt.want {
				t.Errorf("computeDisplayName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestComputeDisplayValue verifies display value computation for different record types.
func TestComputeDisplayValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rs   *dnsv1alpha1.DNSRecordSet
		want string
	}{
		{
			name: "A record single IP",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeA,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}}},
				},
			},
			want: "192.0.2.10",
		},
		{
			name: "A record multiple IPs",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeA,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}},
						{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.11"}},
					},
				},
			},
			want: "192.0.2.10, 192.0.2.11",
		},
		{
			name: "CNAME record",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeCNAME,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "api", CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "api.internal.example.com"}}},
				},
			},
			want: "api.internal.example.com",
		},
		{
			name: "MX records",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeMX,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "@", MX: &dnsv1alpha1.MXRecordSpec{Preference: 10, Exchange: "mail.example.com"}},
					},
				},
			},
			want: "10 mail.example.com",
		},
		{
			name: "TXT record short",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeTXT,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "@", TXT: &dnsv1alpha1.TXTRecordSpec{Content: "v=spf1 include:_spf.example.com ~all"}}},
				},
			},
			want: "\"v=spf1 include:_spf.example.com ~all\"",
		},
		{
			name: "NS record",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeNS,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "sub", NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.example.com"}}},
				},
			},
			want: "ns1.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := computeDisplayValue(tt.rs)
			if got != tt.want {
				t.Errorf("computeDisplayValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// test helpers
// --------------------------------------------------------------------------

// errFakeCreate is a sentinel error returned by fakeEventClient when configured
// to simulate Create failures.
var errFakeCreate = fmt.Errorf("fake create error")

// fakeEventClient implements EventClient for testing.
// It captures created events and can be configured to return errors.
type fakeEventClient struct {
	events []*eventsv1.Event
	err    error
}

func (f *fakeEventClient) Create(
	_ context.Context,
	event *eventsv1.Event,
	_ metav1.CreateOptions,
) (*eventsv1.Event, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.events = append(f.events, event.DeepCopy())
	return event, nil
}
