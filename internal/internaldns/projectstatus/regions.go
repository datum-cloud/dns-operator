// SPDX-License-Identifier: AGPL-3.0-only

package projectstatus

import (
	"slices"
	"sort"
	"strings"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SetRegion replaces region's entry with entry, keeping entries in region
// order. Each region's control plane writes only its own entries, so a region
// that is down leaves the others' entries alone.
func SetRegion(entries *[]dnsv1alpha1.DNSRegionPublicationStatus, entry dnsv1alpha1.DNSRegionPublicationStatus) {
	for i := range *entries {
		if (*entries)[i].Region == entry.Region {
			(*entries)[i] = entry
			return
		}
	}
	*entries = append(*entries, entry)
	sort.Slice(*entries, func(i, j int) bool { return (*entries)[i].Region < (*entries)[j].Region })
}

// UpdateRegion applies change to region's entry, creating it when missing, so
// fields another writer owns (the acknowledgement sink's Available) survive.
func UpdateRegion(entries *[]dnsv1alpha1.DNSRegionPublicationStatus, region string, change func(*dnsv1alpha1.DNSRegionPublicationStatus)) {
	entry := dnsv1alpha1.DNSRegionPublicationStatus{Region: region}
	for _, e := range *entries {
		if e.Region == region {
			entry = e
		}
	}
	change(&entry)
	SetRegion(entries, entry)
}

// RemoveRegion drops region's entry.
func RemoveRegion(entries *[]dnsv1alpha1.DNSRegionPublicationStatus, region string) {
	kept := (*entries)[:0]
	for _, e := range *entries {
		if e.Region != region {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		kept = nil
	}
	*entries = kept
}

// PendingRegions names the expected regions whose entry has not published
// generation, in order.
func PendingRegions(entries []dnsv1alpha1.DNSRegionPublicationStatus, expected []string, generation int64) []string {
	var pending []string
	for _, region := range expected {
		published := false
		for _, e := range entries {
			if e.Region == region && e.ObservedGeneration == generation && e.Published {
				published = true
			}
		}
		if !published {
			pending = append(pending, region)
		}
	}
	sort.Strings(pending)
	return pending
}

// AnyRegion reports whether an expected region's entry for generation
// satisfies ok.
func AnyRegion(entries []dnsv1alpha1.DNSRegionPublicationStatus, expected []string, generation int64, ok func(dnsv1alpha1.DNSRegionPublicationStatus) bool) bool {
	for _, e := range entries {
		if e.ObservedGeneration != generation || !ok(e) {
			continue
		}
		for _, region := range expected {
			if e.Region == region {
				return true
			}
		}
	}
	return false
}

// PublishedInRegions sets Published from the regions still pending, naming
// them so a region that is down is visible without holding the others back.
func PublishedInRegions(conditions *[]metav1.Condition, pending []string, generation int64, now time.Time) bool {
	if len(pending) == 0 {
		return Published(conditions, true, generation, now)
	}
	return set(conditions, "Published", false, "PendingReplicaVerification", "publication awaits required replica verification in "+strings.Join(pending, ", "), generation, now)
}

// RecordRegions records a control plane's verdict for each region it
// publishes (local) that the zone is served in (expected), and drops its
// entries for regions the zone has left. An entry moved to a new publication
// or generation starts unprogrammed and unavailable until change says so.
func RecordRegions(entries *[]dnsv1alpha1.DNSRegionPublicationStatus, local, expected []string, generation, epoch, revision int64, change func(*dnsv1alpha1.DNSRegionPublicationStatus)) {
	for _, region := range local {
		if !slices.Contains(expected, region) {
			RemoveRegion(entries, region)
			continue
		}
		UpdateRegion(entries, region, func(e *dnsv1alpha1.DNSRegionPublicationStatus) {
			if e.WriterEpoch != epoch || e.Revision != revision || e.ObservedGeneration != generation {
				e.Programmed, e.Available = false, false
			}
			e.ObservedGeneration, e.WriterEpoch, e.Revision = generation, epoch, revision
			change(e)
		})
	}
}

// PublishedForRegions sets Published from every expected region's entry. With
// no expected region the zone is served nowhere, so only published counts.
func PublishedForRegions(conditions *[]metav1.Condition, entries []dnsv1alpha1.DNSRegionPublicationStatus, expected []string, published bool, generation int64, now time.Time) bool {
	if len(expected) == 0 {
		return Published(conditions, published, generation, now)
	}
	return PublishedInRegions(conditions, PendingRegions(entries, expected, generation), generation, now)
}
