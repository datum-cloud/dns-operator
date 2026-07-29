// SPDX-License-Identifier: AGPL-3.0-only

package display

import (
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// Activity annotation keys describe the logical hostname-level change on a
// multi-name DNSRecordSet update (issue #72). ActivityPolicy prefers these
// over display-name / display-value for update summaries.
const (
	AnnotationActivityChange = "dns.networking.miloapis.com/activity-change"
	AnnotationActivityName   = "dns.networking.miloapis.com/activity-name"
	AnnotationActivityValue  = "dns.networking.miloapis.com/activity-value"
)

// ActivityChange values stamped on AnnotationActivityChange.
const (
	ActivityChangeAdded   = "added"
	ActivityChangeRemoved = "removed"
	ActivityChangeUpdated = "updated"
)

// ActivityDiff is the hostname-level change between two DNSRecordSet specs.
type ActivityDiff struct {
	Change string
	Name   string
	Value  string
}

// ComputeActivityDiff compares old and new record sets and returns a single
// logical change suitable for Activity summaries. Pure adds → "added", pure
// removes → "removed", pure value changes → "updated". Mixed edits fall back
// to "updated" with all affected names.
func ComputeActivityDiff(oldRS, newRS *dnsv1alpha1.DNSRecordSet, zoneDomainName string) ActivityDiff {
	if newRS == nil {
		return ActivityDiff{}
	}
	if oldRS == nil {
		return ActivityDiff{}
	}

	oldByName := entriesByName(oldRS)
	newByName := entriesByName(newRS)

	var added, removed, updated []string
	for name := range newByName {
		if _, ok := oldByName[name]; !ok {
			added = append(added, name)
			continue
		}
		if signatureForName(oldRS, name) != signatureForName(newRS, name) {
			updated = append(updated, name)
		}
	}
	for name := range oldByName {
		if _, ok := newByName[name]; !ok {
			removed = append(removed, name)
		}
	}

	// Stable order matching UniqueRecordNames / first-occurrence in new, then old.
	added = orderNames(UniqueRecordNames(newRS), added)
	updated = orderNames(UniqueRecordNames(newRS), updated)
	removed = orderNames(UniqueRecordNames(oldRS), removed)

	switch {
	case len(added) > 0 && len(removed) == 0 && len(updated) == 0:
		return ActivityDiff{
			Change: ActivityChangeAdded,
			Name:   fqdnsForNames(added, zoneDomainName),
			Value:  displayValueForNames(newRS, added),
		}
	case len(removed) > 0 && len(added) == 0 && len(updated) == 0:
		return ActivityDiff{
			Change: ActivityChangeRemoved,
			Name:   fqdnsForNames(removed, zoneDomainName),
			Value:  displayValueForNames(oldRS, removed),
		}
	case len(updated) > 0 && len(added) == 0 && len(removed) == 0:
		return ActivityDiff{
			Change: ActivityChangeUpdated,
			Name:   fqdnsForNames(updated, zoneDomainName),
			Value:  displayValueForNames(newRS, updated),
		}
	case len(added) == 0 && len(removed) == 0 && len(updated) == 0:
		return ActivityDiff{}
	default:
		// Mixed add/remove/update in one write — summarize as updated.
		affected := append(append(append([]string{}, added...), removed...), updated...)
		affected = dedupePreserveOrder(affected)
		src := newRS
		if len(newByName) == 0 {
			src = oldRS
		}
		return ActivityDiff{
			Change: ActivityChangeUpdated,
			Name:   fqdnsForNames(affected, zoneDomainName),
			Value:  displayValueForNames(src, intersectNames(affected, UniqueRecordNames(src))),
		}
	}
}

// EnsureActivityAnnotations stamps or clears activity-* annotations based on
// the diff between oldRS and rs. Returns true if annotations changed.
func EnsureActivityAnnotations(rs, oldRS *dnsv1alpha1.DNSRecordSet, zoneDomainName string) bool {
	if rs == nil {
		return false
	}
	diff := ComputeActivityDiff(oldRS, rs, zoneDomainName)
	if diff.Change == "" {
		return ClearActivityAnnotations(rs)
	}

	if rs.Annotations == nil {
		rs.Annotations = make(map[string]string)
	}

	changed := false
	if rs.Annotations[AnnotationActivityChange] != diff.Change {
		rs.Annotations[AnnotationActivityChange] = diff.Change
		changed = true
	}
	if rs.Annotations[AnnotationActivityName] != diff.Name {
		rs.Annotations[AnnotationActivityName] = diff.Name
		changed = true
	}
	if rs.Annotations[AnnotationActivityValue] != diff.Value {
		rs.Annotations[AnnotationActivityValue] = diff.Value
		changed = true
	}
	return changed
}

// ClearActivityAnnotations removes activity-* annotations. Returns true if any
// were present.
func ClearActivityAnnotations(rs *dnsv1alpha1.DNSRecordSet) bool {
	if rs == nil || rs.Annotations == nil {
		return false
	}
	changed := false
	for _, key := range []string{AnnotationActivityChange, AnnotationActivityName, AnnotationActivityValue} {
		if _, ok := rs.Annotations[key]; ok {
			delete(rs.Annotations, key)
			changed = true
		}
	}
	return changed
}

func entriesByName(rs *dnsv1alpha1.DNSRecordSet) map[string][]dnsv1alpha1.RecordEntry {
	out := make(map[string][]dnsv1alpha1.RecordEntry)
	if rs == nil {
		return out
	}
	for _, r := range rs.Spec.Records {
		out[r.Name] = append(out[r.Name], r)
	}
	return out
}

func subsetForNames(rs *dnsv1alpha1.DNSRecordSet, names []string) *dnsv1alpha1.DNSRecordSet {
	want := make(map[string]struct{}, len(names))
	for _, n := range names {
		want[n] = struct{}{}
	}
	sub := &dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: rs.Spec.RecordType,
		},
	}
	for _, r := range rs.Spec.Records {
		if _, ok := want[r.Name]; ok {
			sub.Spec.Records = append(sub.Spec.Records, r)
		}
	}
	return sub
}

func signatureForName(rs *dnsv1alpha1.DNSRecordSet, name string) string {
	return ComputeDisplayValue(subsetForNames(rs, []string{name}))
}

func displayValueForNames(rs *dnsv1alpha1.DNSRecordSet, names []string) string {
	if len(names) == 0 {
		return ""
	}
	return ComputeDisplayValue(subsetForNames(rs, names))
}

func fqdnsForNames(names []string, zoneDomainName string) string {
	fqdns := make([]string, 0, len(names))
	for _, name := range names {
		fqdns = append(fqdns, BuildFQDN(name, zoneDomainName))
	}
	return strings.Join(fqdns, ", ")
}

func orderNames(preferredOrder, selected []string) []string {
	if len(selected) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(selected))
	for _, n := range selected {
		want[n] = struct{}{}
	}
	out := make([]string, 0, len(selected))
	for _, n := range preferredOrder {
		if _, ok := want[n]; ok {
			out = append(out, n)
			delete(want, n)
		}
	}
	for _, n := range selected {
		if _, ok := want[n]; ok {
			out = append(out, n)
			delete(want, n)
		}
	}
	return out
}

func dedupePreserveOrder(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

func intersectNames(names, available []string) []string {
	avail := make(map[string]struct{}, len(available))
	for _, n := range available {
		avail[n] = struct{}{}
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if _, ok := avail[n]; ok {
			out = append(out, n)
		}
	}
	return out
}
