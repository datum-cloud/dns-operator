// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// Annotation keys used by the DNS service for ActivityPolicy event matching.
// These are DNS service-specific annotations that the DNS service configures
// via ActivityPolicy to generate human-readable activity summaries.
const (
	AnnotationEventType          = "dns.networking.miloapis.com/event-type"
	AnnotationObservedGeneration = "dns.networking.miloapis.com/observed-generation"
	AnnotationDomainName         = "dns.networking.miloapis.com/domain-name"
	AnnotationRecordType         = "dns.networking.miloapis.com/record-type"

	// Resource identity annotations present on all events.
	AnnotationResourceName      = "dns.networking.miloapis.com/resource-name"
	AnnotationResourceNamespace = "dns.networking.miloapis.com/resource-namespace"

	// DNSZone-specific annotations.
	AnnotationZoneClass   = "dns.networking.miloapis.com/zone-class"
	AnnotationNameservers = "dns.networking.miloapis.com/nameservers"

	// DNSRecordSet-specific annotations.
	AnnotationZoneRef     = "dns.networking.miloapis.com/zone-ref"
	AnnotationRecordCount = "dns.networking.miloapis.com/record-count"
	AnnotationRecordName  = "dns.networking.miloapis.com/record-name"
	AnnotationIPAddresses = "dns.networking.miloapis.com/ip-addresses"

	// Shared failure annotation.
	AnnotationFailureReason = "dns.networking.miloapis.com/failure-reason"
)

// Kubernetes Event reason codes (PascalCase per k8s convention).
// CRUD lifecycle events (created/updated/deleted) are intentionally absent;
// those are captured by audit logs. Only async outcome events that audit logs
// cannot capture are emitted here.
const (
	// DNSZone event reasons.
	EventReasonZoneProgrammed        = "ZoneProgrammed"
	EventReasonZoneProgrammingFailed = "ZoneProgrammingFailed"

	// DNSRecordSet event reasons.
	EventReasonRecordSetProgrammed        = "RecordSetProgrammed"
	EventReasonRecordSetProgrammingFailed = "RecordSetProgrammingFailed"
)

// Activity type strings (dot-notation for platform Activity selector matching).
// CRUD lifecycle activity types (created/updated/deleted) are intentionally absent;
// those are captured by audit logs. Only async outcome events that audit logs
// cannot capture are emitted here.
const (
	// DNSZone activity types.
	ActivityTypeZoneProgrammed        = "dns.zone.programmed"
	ActivityTypeZoneProgrammingFailed = "dns.zone.programming_failed"

	// DNSRecordSet activity types.
	ActivityTypeRecordSetProgrammed        = "dns.recordset.programmed"
	ActivityTypeRecordSetProgrammingFailed = "dns.recordset.programming_failed"
)

// ZoneEventData holds all the data needed to emit an annotated activity event
// for a DNSZone. Nameservers and FailReason are optional and only used for
// specific event types (programmed and programming_failed respectively).
type ZoneEventData struct {
	Zone        *dnsv1alpha1.DNSZone
	Nameservers []string // for programmed events; mirrors zone.Status.Nameservers
	FailReason  string   // for programming_failed events; from condition Message
}

// RecordSetEventData holds all the data needed to emit an annotated activity
// event for a DNSRecordSet. DomainName and FailReason are optional.
type RecordSetEventData struct {
	RecordSet  *dnsv1alpha1.DNSRecordSet
	DomainName string // parent zone's domain; from zone.Spec.DomainName
	FailReason string // for programming_failed events; from condition Message
}

// recordZoneActivityEvent emits an annotated Kubernetes event for a DNSZone.
// It attaches resource identity, domain-name, zone-class, and (where available)
// nameservers and failure-reason annotations so the DNS service ActivityPolicy
// can filter, route and template the event.
func recordZoneActivityEvent(
	recorder record.EventRecorder,
	zone *dnsv1alpha1.DNSZone,
	eventType, reason, activityType string,
	messageFmt string,
	args ...interface{},
) {
	recordZoneActivityEventWithData(recorder, ZoneEventData{Zone: zone}, eventType, reason, activityType, messageFmt, args...)
}

// recordZoneActivityEventWithData is the full-featured variant that accepts a
// ZoneEventData struct containing optional nameservers and failure reason.
func recordZoneActivityEventWithData(
	recorder record.EventRecorder,
	data ZoneEventData,
	eventType, reason, activityType string,
	messageFmt string,
	args ...interface{},
) {
	if recorder == nil {
		return
	}
	zone := data.Zone
	annotations := map[string]string{
		AnnotationEventType:          activityType,
		AnnotationObservedGeneration: strconv.FormatInt(zone.Generation, 10),
		AnnotationDomainName:         zone.Spec.DomainName,
		AnnotationResourceName:       zone.Name,
		AnnotationResourceNamespace:  zone.Namespace,
	}
	if zone.Spec.DNSZoneClassName != "" {
		annotations[AnnotationZoneClass] = zone.Spec.DNSZoneClassName
	}
	if len(data.Nameservers) > 0 {
		annotations[AnnotationNameservers] = strings.Join(data.Nameservers, ",")
	}
	if data.FailReason != "" {
		annotations[AnnotationFailureReason] = data.FailReason
	}
	recorder.AnnotatedEventf(zone, annotations, eventType, reason, messageFmt, args...)
}

// recordRecordSetActivityEvent emits an annotated Kubernetes event for a
// DNSRecordSet. It attaches resource identity and record-type annotations so
// the DNS service ActivityPolicy can filter events by DNS record type.
func recordRecordSetActivityEvent(
	recorder record.EventRecorder,
	rs *dnsv1alpha1.DNSRecordSet,
	eventType, reason, activityType string,
	messageFmt string,
	args ...interface{},
) {
	recordRecordSetActivityEventWithData(recorder, RecordSetEventData{RecordSet: rs}, eventType, reason, activityType, messageFmt, args...)
}

// recordRecordSetActivityEventWithData is the full-featured variant that accepts
// a RecordSetEventData struct containing optional domain name and failure reason.
func recordRecordSetActivityEventWithData(
	recorder record.EventRecorder,
	data RecordSetEventData,
	eventType, reason, activityType string,
	messageFmt string,
	args ...interface{},
) {
	if recorder == nil {
		return
	}
	rs := data.RecordSet
	annotations := map[string]string{
		AnnotationEventType:          activityType,
		AnnotationObservedGeneration: strconv.FormatInt(rs.Generation, 10),
		AnnotationRecordType:         string(rs.Spec.RecordType),
		AnnotationResourceName:       rs.Name,
		AnnotationResourceNamespace:  rs.Namespace,
		AnnotationZoneRef:            rs.Spec.DNSZoneRef.Name,
	}
	if data.DomainName != "" {
		annotations[AnnotationDomainName] = data.DomainName
	}
	if len(rs.Spec.Records) > 0 {
		annotations[AnnotationRecordCount] = strconv.Itoa(len(rs.Spec.Records))
		// Use the first record entry's name as the record name annotation.
		annotations[AnnotationRecordName] = rs.Spec.Records[0].Name
	}
	// Extract IP addresses for A/AAAA record types.
	if ips := extractIPAddresses(rs); len(ips) > 0 {
		annotations[AnnotationIPAddresses] = strings.Join(ips, ",")
	}
	if data.FailReason != "" {
		annotations[AnnotationFailureReason] = data.FailReason
	}
	recorder.AnnotatedEventf(rs, annotations, eventType, reason, messageFmt, args...)
}

// extractIPAddresses collects IP address content values from A and AAAA record
// entries in the given DNSRecordSet. Returns nil for all other record types.
func extractIPAddresses(rs *dnsv1alpha1.DNSRecordSet) []string {
	switch rs.Spec.RecordType {
	case dnsv1alpha1.RRTypeA:
		var ips []string
		for _, r := range rs.Spec.Records {
			if r.A != nil && r.A.Content != "" {
				ips = append(ips, r.A.Content)
			}
		}
		return ips
	case dnsv1alpha1.RRTypeAAAA:
		var ips []string
		for _, r := range rs.Spec.Records {
			if r.AAAA != nil && r.AAAA.Content != "" {
				ips = append(ips, r.AAAA.Content)
			}
		}
		return ips
	default:
		return nil
	}
}

// programmedConditionTransitioned returns true when the Programmed condition changed
// in a way that warrants an event:
//   - nil/non-True -> True: emit "programmed" (first successful programming)
//   - True -> False: emit "programming_failed"
//   - nil -> False: returns false (no event for first-time failures before any success)
//   - curr nil: returns false (condition removed, no event)
func programmedConditionTransitioned(prev, curr *metav1.Condition) bool {
	if curr == nil {
		return false
	}
	if curr.Status == metav1.ConditionTrue {
		// Emit on any transition to True (including first-time nil->True)
		prevStatus := metav1.ConditionFalse
		if prev != nil {
			prevStatus = prev.Status
		}
		return prevStatus != metav1.ConditionTrue
	}
	// Emit True->False only; nil->False and False->False do not trigger
	if curr.Status == metav1.ConditionFalse {
		return prev != nil && prev.Status == metav1.ConditionTrue
	}
	return false
}
