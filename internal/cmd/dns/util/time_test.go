// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRelativeAge(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name string
		in   metav1.Time
		want string
	}{
		{"seconds", metav1.NewTime(now.Add(-45 * time.Second)), "45s"},
		{"just now", metav1.NewTime(now), "0s"},
		{"minutes", metav1.NewTime(now.Add(-12 * time.Minute)), "12m"},
		{"minute boundary", metav1.NewTime(now.Add(-60 * time.Second)), "1m"},
		{"hours", metav1.NewTime(now.Add(-3 * time.Hour)), "3h"},
		{"days", metav1.NewTime(now.Add(-48 * time.Hour)), "2d"},
		{"weeks", metav1.NewTime(now.Add(-14 * 24 * time.Hour)), "14d"},
		{"future clamps to zero", metav1.NewTime(now.Add(5 * time.Second)), "0s"},
		{"zero value", metav1.Time{}, "—"},
		{"unix epoch", metav1.NewTime(time.Unix(0, 0)), "—"},
		{"crd default epoch", metav1.NewTime(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)), "—"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RelativeAge(tc.in); got != tc.want {
				t.Errorf("RelativeAge() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRelativeAgeVerbose(t *testing.T) {
	tests := []struct {
		name string
		in   metav1.Time
		want string
	}{
		{"normal age gets a suffix", metav1.NewTime(time.Now().Add(-3 * time.Hour)), "3h ago"},
		{"epoch stays a dash", metav1.NewTime(time.Unix(0, 0)), "—"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RelativeAgeVerbose(tc.in); got != tc.want {
				t.Errorf("RelativeAgeVerbose() = %q, want %q", got, tc.want)
			}
		})
	}
}
