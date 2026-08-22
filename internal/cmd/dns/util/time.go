// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RelativeAge returns a compact age string for table cells (no "ago" suffix).
//
//	< 60s  → "Xs"
//	< 60m  → "Xm"
//	< 24h  → "Xh"
//	>= 24h → "Xd"
//
// A zero or Unix-epoch timestamp renders as an em dash. DNSRecordSet's CRD
// default seeds its conditions with lastTransitionTime 1970-01-01T00:00:00Z, so
// a freshly created record set would otherwise report an age of 56 years.
func RelativeAge(t metav1.Time) string {
	if IsNeverTransitioned(t) {
		return "—"
	}

	d := time.Since(t.Time)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// RelativeAgeVerbose returns an age string with an "ago" suffix for detail
// views. A never-transitioned timestamp stays a bare em dash.
func RelativeAgeVerbose(t metav1.Time) string {
	age := RelativeAge(t)
	if age == "—" {
		return age
	}
	return age + " ago"
}

// IsNeverTransitioned reports whether t is the zero value or the Unix epoch, the
// two ways the API expresses "this never happened".
func IsNeverTransitioned(t metav1.Time) bool {
	return t.IsZero() || t.Time.UTC().Unix() == 0
}
