// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
)

// RelativeAge returns a compact age string for table cells (no "ago" suffix),
// in the same shape kubectl's AGE column uses, so an age read here matches an
// age read anywhere else in the ecosystem.
//
// A zero or Unix-epoch timestamp renders as an em dash rather than an age. The
// API expresses "this never happened" as the epoch, and without the guard a
// freshly created object would report an age of half a century.
func RelativeAge(t metav1.Time) string {
	if IsNeverTransitioned(t) {
		return emDash
	}

	d := time.Since(t.Time)
	if d < 0 {
		d = 0
	}
	return duration.HumanDuration(d)
}

// RelativeAgeVerbose returns an age string with an "ago" suffix for detail
// views. A never-transitioned timestamp stays a bare em dash.
func RelativeAgeVerbose(t metav1.Time) string {
	age := RelativeAge(t)
	if age == emDash {
		return age
	}
	return age + " ago"
}

// IsNeverTransitioned reports whether t is the zero value or the Unix epoch, the
// two ways the API expresses "this never happened".
func IsNeverTransitioned(t metav1.Time) bool {
	return t.IsZero() || t.Time.UTC().Unix() == 0
}
