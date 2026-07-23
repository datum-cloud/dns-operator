// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/display"
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
	AnnotationRecordNames = "dns.networking.miloapis.com/record-names"
	AnnotationIPAddresses = "dns.networking.miloapis.com/ip-addresses"

	// Display annotations for human-readable activity summaries.
	// Defined in internal/display; aliased here for existing callers/tests.
	AnnotationDisplayName  = display.AnnotationDisplayName
	AnnotationDisplayValue = display.AnnotationDisplayValue

	// Type-specific event annotations for record values.
	// These supplement ip-addresses for non-IP record types.
	AnnotationCNAMETarget = "dns.networking.miloapis.com/cname-target"
	AnnotationMXHosts     = "dns.networking.miloapis.com/mx-hosts"

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

// EventClient is the minimal interface for creating events.k8s.io/v1 Events.
// Obtained via kubernetes.Clientset.EventsV1().Events(namespace).
// Defined as an interface to allow test fakes without a live API server.
type EventClient interface {
	Create(ctx context.Context, event *eventsv1.Event, opts metav1.CreateOptions) (*eventsv1.Event, error)
}

// ZoneEventData holds all the data needed to emit an annotated activity event
// for a DNSZone. Nameservers and FailReason are optional and only used for
// specific event types (programmed and programming_failed respectively).
type ZoneEventData struct {
	Zone        *dnsv1alpha1.DNSZone
	Nameservers []string // for programmed events; mirrors zone.Status.Nameservers
	FailReason  string   // for programming_failed events; from condition Message
}

// RecordSetEventData holds all the data needed to emit a direct events.k8s.io/v1
// Event for a DNSRecordSet. Zone is required for setting event.related.
type RecordSetEventData struct {
	RecordSet  *dnsv1alpha1.DNSRecordSet
	Zone       *dnsv1alpha1.DNSZone // parent zone; populates event.related
	DomainName string               // from zone.Spec.DomainName; optional if Zone is set
	FailReason string               // for programming_failed events; from condition Message
}

// emitRecordSetEvent creates an events.k8s.io/v1 Event for a DNSRecordSet.
// The event.regarding is set to the DNSRecordSet; event.related is set to the
// parent DNSZone so activity timeline links navigate to the zone detail page.
// If eventClient is nil or if Create fails, the error is logged and swallowed.
func emitRecordSetEvent(
	ctx context.Context,
	eventClient EventClient,
	data RecordSetEventData,
	eventType, reason, activityType string,
	messageFmt string,
	args ...any,
) {
	if eventClient == nil {
		return
	}
	rs := data.RecordSet
	domainName := data.DomainName
	if domainName == "" && data.Zone != nil {
		domainName = data.Zone.Spec.DomainName
	}

	annotations := map[string]string{
		AnnotationEventType:          activityType,
		AnnotationObservedGeneration: strconv.FormatInt(rs.Generation, 10),
		AnnotationRecordType:         string(rs.Spec.RecordType),
		AnnotationResourceName:       rs.Name,
		AnnotationResourceNamespace:  rs.Namespace,
		AnnotationZoneRef:            rs.Spec.DNSZoneRef.Name,
	}
	if domainName != "" {
		annotations[AnnotationDomainName] = domainName
	}
	if len(rs.Spec.Records) > 0 {
		annotations[AnnotationRecordCount] = strconv.Itoa(len(rs.Spec.Records))
		annotations[AnnotationRecordNames] = strings.Join(display.UniqueRecordNames(rs), ",")
	}
	if ips := display.ExtractIPAddresses(rs); len(ips) > 0 {
		annotations[AnnotationIPAddresses] = strings.Join(ips, ",")
	}
	switch rs.Spec.RecordType {
	case dnsv1alpha1.RRTypeCNAME:
		if target := display.ExtractCNAMETarget(rs); target != "" {
			annotations[AnnotationCNAMETarget] = target
		}
	case dnsv1alpha1.RRTypeMX:
		if hosts := display.ExtractMXHosts(rs); hosts != "" {
			annotations[AnnotationMXHosts] = hosts
		}
	}
	if domainName != "" {
		annotations[AnnotationDisplayName] = display.ComputeDisplayName(rs, domainName)
	}
	if data.FailReason != "" {
		annotations[AnnotationFailureReason] = data.FailReason
	}

	evt := buildEvent(rs.Namespace, rs.Name, annotations, eventType, reason,
		fmt.Sprintf(messageFmt, args...))
	evt.Regarding = recordSetObjectRef(rs)
	if data.Zone != nil {
		zRef := zoneObjectRef(data.Zone)
		evt.Related = &zRef
	}

	if _, err := eventClient.Create(ctx, evt, metav1.CreateOptions{}); err != nil {
		log.FromContext(ctx).Error(err, "failed to emit RecordSet event",
			"reason", reason, "recordset", rs.Name)
	}
}

// emitZoneEvent creates an events.k8s.io/v1 Event for a DNSZone.
// The event.regarding is set to the DNSZone; event.related is nil.
// If eventClient is nil or if Create fails, the error is logged and swallowed.
func emitZoneEvent(
	ctx context.Context,
	eventClient EventClient,
	data ZoneEventData,
	eventType, reason, activityType string,
	messageFmt string,
	args ...any,
) {
	if eventClient == nil {
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

	evt := buildEvent(zone.Namespace, zone.Name, annotations, eventType, reason,
		fmt.Sprintf(messageFmt, args...))
	evt.Regarding = zoneObjectRef(zone)

	if _, err := eventClient.Create(ctx, evt, metav1.CreateOptions{}); err != nil {
		log.FromContext(ctx).Error(err, "failed to emit Zone event",
			"reason", reason, "zone", zone.Name)
	}
}

// buildEvent constructs an eventsv1.Event with a unique name and common fields.
// Regarding and Related must be set by the caller.
func buildEvent(
	namespace, subjectName string,
	annotations map[string]string,
	eventType, reason, note string,
) *eventsv1.Event {
	// Unique name: <subject>.<nanosecond-hex> avoids conflicts on rapid re-emission.
	name := fmt.Sprintf("%s.%x", subjectName, time.Now().UnixNano())
	// Kubernetes name length limit is 253 characters. Trim the subject prefix if needed.
	if len(name) > 253 {
		name = name[len(name)-253:]
	}
	return &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
		EventTime:           metav1.NewMicroTime(time.Now()),
		Action:              reason,
		Reason:              reason,
		Note:                note,
		Type:                eventType,
		ReportingController: "dns.networking.miloapis.com/dns-operator",
		ReportingInstance:   reportingInstance(),
	}
}

// reportingInstance returns a non-empty identifier for the reporting pod.
// events.k8s.io/v1 requires reportingInstance to be set (1-128 chars), so we
// fall back through POD_NAME -> hostname -> a constant when the downward-API
// env var is unset (e.g. in tests or misconfigured deployments). The result is
// truncated to the 128-character API limit.
func reportingInstance() string {
	name := os.Getenv("POD_NAME")
	if name == "" {
		if h, err := os.Hostname(); err == nil {
			name = h
		}
	}
	if name == "" {
		name = "dns-operator"
	}
	if len(name) > 128 {
		name = name[:128]
	}
	return name
}

// recordSetObjectRef builds a corev1.ObjectReference for a DNSRecordSet using
// hardcoded GVK values (stable and known for this operator).
func recordSetObjectRef(rs *dnsv1alpha1.DNSRecordSet) corev1.ObjectReference {
	return corev1.ObjectReference{
		APIVersion: dnsv1alpha1.GroupVersion.String(),
		Kind:       "DNSRecordSet",
		Namespace:  rs.Namespace,
		Name:       rs.Name,
		UID:        rs.UID,
	}
}

// zoneObjectRef builds a corev1.ObjectReference for a DNSZone using hardcoded
// GVK values (stable and known for this operator).
func zoneObjectRef(zone *dnsv1alpha1.DNSZone) corev1.ObjectReference {
	return corev1.ObjectReference{
		APIVersion: dnsv1alpha1.GroupVersion.String(),
		Kind:       "DNSZone",
		Namespace:  zone.Namespace,
		Name:       zone.Name,
		UID:        zone.UID,
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
