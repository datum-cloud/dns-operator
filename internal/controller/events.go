// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"fmt"
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
	AnnotationRecordNames = "dns.networking.miloapis.com/record-names"
	AnnotationIPAddresses = "dns.networking.miloapis.com/ip-addresses"

	// Display annotations for human-readable activity summaries.
	// These are set on DNSRecordSet resources by the controller for use in
	// ActivityPolicy audit rule templates. They provide pre-formatted
	// user-friendly values that would be complex to compute in CEL.
	AnnotationDisplayName  = "dns.networking.miloapis.com/display-name"
	AnnotationDisplayValue = "dns.networking.miloapis.com/display-value"

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
		// Collect unique record names preserving order of first occurrence.
		annotations[AnnotationRecordNames] = strings.Join(uniqueRecordNames(rs), ",")
	}
	// Extract IP addresses for A/AAAA record types.
	if ips := extractIPAddresses(rs); len(ips) > 0 {
		annotations[AnnotationIPAddresses] = strings.Join(ips, ",")
	}
	// Extract type-specific record values for activity summaries.
	switch rs.Spec.RecordType {
	case dnsv1alpha1.RRTypeCNAME:
		if target := extractCNAMETarget(rs); target != "" {
			annotations[AnnotationCNAMETarget] = target
		}
	case dnsv1alpha1.RRTypeMX:
		if hosts := extractMXHosts(rs); hosts != "" {
			annotations[AnnotationMXHosts] = hosts
		}
	}
	// Add display-name annotation (pre-computed FQDN) for easier template usage.
	if data.DomainName != "" {
		annotations[AnnotationDisplayName] = computeDisplayName(rs, data.DomainName)
	}
	if data.FailReason != "" {
		annotations[AnnotationFailureReason] = data.FailReason
	}
	recorder.AnnotatedEventf(rs, annotations, eventType, reason, messageFmt, args...)
}

// uniqueRecordNames returns a deduplicated list of record names from the
// DNSRecordSet, preserving the order of first occurrence. This handles the
// common case where multiple records share the same name (e.g., round-robin A
// records) as well as the less common case of different names in one set.
func uniqueRecordNames(rs *dnsv1alpha1.DNSRecordSet) []string {
	seen := make(map[string]struct{})
	var names []string
	for _, r := range rs.Spec.Records {
		if _, ok := seen[r.Name]; !ok {
			seen[r.Name] = struct{}{}
			names = append(names, r.Name)
		}
	}
	return names
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

// extractCNAMETarget returns the CNAME target from the first record entry.
// Returns empty string if not a CNAME record or if no valid target exists.
func extractCNAMETarget(rs *dnsv1alpha1.DNSRecordSet) string {
	if rs.Spec.RecordType != dnsv1alpha1.RRTypeCNAME {
		return ""
	}
	if len(rs.Spec.Records) > 0 && rs.Spec.Records[0].CNAME != nil {
		return rs.Spec.Records[0].CNAME.Content
	}
	return ""
}

// extractMXHosts returns a formatted string of MX hosts with their preferences.
// Format: "10 mail.example.com, 20 mail2.example.com"
func extractMXHosts(rs *dnsv1alpha1.DNSRecordSet) string {
	if rs.Spec.RecordType != dnsv1alpha1.RRTypeMX {
		return ""
	}
	var hosts []string
	for _, r := range rs.Spec.Records {
		if r.MX != nil && r.MX.Exchange != "" {
			hosts = append(hosts, fmt.Sprintf("%d %s", r.MX.Preference, r.MX.Exchange))
		}
	}
	return strings.Join(hosts, ", ")
}

// computeDisplayName returns a human-friendly name for the DNSRecordSet.
// For most records this is the FQDN (e.g., "www.example.com").
// For multiple unique names, returns comma-separated FQDNs.
func computeDisplayName(rs *dnsv1alpha1.DNSRecordSet, zoneDomainName string) string {
	names := uniqueRecordNames(rs)
	if len(names) == 0 {
		return ""
	}
	var fqdns []string
	for _, name := range names {
		fqdn := buildFQDN(name, zoneDomainName)
		fqdns = append(fqdns, fqdn)
	}
	return strings.Join(fqdns, ", ")
}

// computeDisplayValue returns a human-friendly value for the DNSRecordSet
// based on its record type. This is used in activity summaries to show
// what the record points to (IP addresses, CNAME targets, MX hosts, etc.).
func computeDisplayValue(rs *dnsv1alpha1.DNSRecordSet) string {
	const maxLength = 200

	switch rs.Spec.RecordType {
	case dnsv1alpha1.RRTypeA, dnsv1alpha1.RRTypeAAAA:
		ips := extractIPAddresses(rs)
		result := strings.Join(ips, ", ")
		if len(result) > maxLength {
			return result[:maxLength-3] + "..."
		}
		return result

	case dnsv1alpha1.RRTypeCNAME:
		return extractCNAMETarget(rs)

	case dnsv1alpha1.RRTypeALIAS:
		if len(rs.Spec.Records) > 0 && rs.Spec.Records[0].ALIAS != nil {
			return rs.Spec.Records[0].ALIAS.Content
		}
		return ""

	case dnsv1alpha1.RRTypeMX:
		result := extractMXHosts(rs)
		if len(result) > maxLength {
			return result[:maxLength-3] + "..."
		}
		return result

	case dnsv1alpha1.RRTypeTXT:
		if len(rs.Spec.Records) > 0 && rs.Spec.Records[0].TXT != nil {
			content := rs.Spec.Records[0].TXT.Content
			if len(content) > 60 {
				return fmt.Sprintf("\"%s...\"", content[:57])
			}
			return fmt.Sprintf("\"%s\"", content)
		}
		return ""

	case dnsv1alpha1.RRTypeNS:
		var servers []string
		for _, r := range rs.Spec.Records {
			if r.NS != nil && r.NS.Content != "" {
				servers = append(servers, r.NS.Content)
			}
		}
		result := strings.Join(servers, ", ")
		if len(result) > maxLength {
			return result[:maxLength-3] + "..."
		}
		return result

	case dnsv1alpha1.RRTypeSRV:
		var entries []string
		for _, r := range rs.Spec.Records {
			if r.SRV != nil {
				entries = append(entries, fmt.Sprintf("%d %d %d %s",
					r.SRV.Priority, r.SRV.Weight, r.SRV.Port, r.SRV.Target))
			}
		}
		result := strings.Join(entries, ", ")
		if len(result) > maxLength {
			return result[:maxLength-3] + "..."
		}
		return result

	default:
		if len(rs.Spec.Records) > 1 {
			return fmt.Sprintf("%d records", len(rs.Spec.Records))
		}
		return "(see details)"
	}
}

// buildFQDN constructs a fully-qualified domain name from a record name
// and zone domain. Handles the special "@" name for zone apex.
func buildFQDN(recordName, zoneDomainName string) string {
	if recordName == "@" {
		return zoneDomainName
	}
	return recordName + "." + zoneDomainName
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
