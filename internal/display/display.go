// SPDX-License-Identifier: AGPL-3.0-only

// Package display provides human-friendly DNSRecordSet display helpers used by
// ActivityPolicy annotations (set at admission and by the replicator).
package display

import (
	"fmt"
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// Annotation keys for human-readable activity summaries.
const (
	AnnotationDisplayName  = "dns.networking.miloapis.com/display-name"
	AnnotationDisplayValue = "dns.networking.miloapis.com/display-value"
)

// EnsureAnnotations sets display-name and display-value on the DNSRecordSet when
// they are missing or outdated. Returns true if annotations were modified.
func EnsureAnnotations(rs *dnsv1alpha1.DNSRecordSet, zoneDomainName string) bool {
	expectedDisplayName := ComputeDisplayName(rs, zoneDomainName)
	expectedDisplayValue := ComputeDisplayValue(rs)

	if rs.Annotations == nil {
		rs.Annotations = make(map[string]string)
	}

	currentDisplayName := rs.Annotations[AnnotationDisplayName]
	currentDisplayValue := rs.Annotations[AnnotationDisplayValue]

	if currentDisplayName == expectedDisplayName && currentDisplayValue == expectedDisplayValue {
		return false
	}

	rs.Annotations[AnnotationDisplayName] = expectedDisplayName
	rs.Annotations[AnnotationDisplayValue] = expectedDisplayValue
	return true
}

// UniqueRecordNames returns a deduplicated list of record names, preserving
// first-occurrence order.
func UniqueRecordNames(rs *dnsv1alpha1.DNSRecordSet) []string {
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

// ExtractIPAddresses collects IP address content values from A and AAAA records.
// Returns nil for all other record types.
func ExtractIPAddresses(rs *dnsv1alpha1.DNSRecordSet) []string {
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

// ExtractCNAMETarget returns the CNAME target from the first record entry.
func ExtractCNAMETarget(rs *dnsv1alpha1.DNSRecordSet) string {
	if rs.Spec.RecordType != dnsv1alpha1.RRTypeCNAME {
		return ""
	}
	if len(rs.Spec.Records) > 0 && rs.Spec.Records[0].CNAME != nil {
		return rs.Spec.Records[0].CNAME.Content
	}
	return ""
}

// ExtractMXHosts returns a formatted string of MX hosts with preferences.
// Format: "10 mail.example.com, 20 mail2.example.com"
func ExtractMXHosts(rs *dnsv1alpha1.DNSRecordSet) string {
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

// ComputeDisplayName returns a human-friendly name for the DNSRecordSet.
// For most records this is the FQDN (e.g., "www.example.com").
func ComputeDisplayName(rs *dnsv1alpha1.DNSRecordSet, zoneDomainName string) string {
	names := UniqueRecordNames(rs)
	if len(names) == 0 {
		return ""
	}
	fqdns := make([]string, 0, len(names))
	for _, name := range names {
		fqdns = append(fqdns, BuildFQDN(name, zoneDomainName))
	}
	return strings.Join(fqdns, ", ")
}

// ComputeDisplayValue returns a human-friendly value for the DNSRecordSet
// based on its record type (IPs, CNAME targets, MX hosts, etc.).
func ComputeDisplayValue(rs *dnsv1alpha1.DNSRecordSet) string {
	const maxLength = 200

	switch rs.Spec.RecordType {
	case dnsv1alpha1.RRTypeA, dnsv1alpha1.RRTypeAAAA:
		ips := ExtractIPAddresses(rs)
		result := strings.Join(ips, ", ")
		if len(result) > maxLength {
			return result[:maxLength-3] + "..."
		}
		return result

	case dnsv1alpha1.RRTypeCNAME:
		return ExtractCNAMETarget(rs)

	case dnsv1alpha1.RRTypeALIAS:
		if len(rs.Spec.Records) > 0 && rs.Spec.Records[0].ALIAS != nil {
			return rs.Spec.Records[0].ALIAS.Content
		}
		return ""

	case dnsv1alpha1.RRTypeMX:
		result := ExtractMXHosts(rs)
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

// BuildFQDN constructs a fully-qualified domain name from a record name and
// zone domain. Handles the special "@" name for zone apex.
func BuildFQDN(recordName, zoneDomainName string) string {
	if recordName == "@" {
		return zoneDomainName
	}
	return recordName + "." + zoneDomainName
}
