// SPDX-License-Identifier: AGPL-3.0-only

package ownername

import "testing"

func TestQualify(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		owner, zone, want string
	}{
		{"@", "example.com", "example.com."},
		{"", "example.com", "example.com."},
		{"www", "example.com", "www.example.com."},
		{"WWW", "example.com", "www.example.com."},
		{"www", "EXAMPLE.COM", "www.example.com."},
		{"abs.example.", "example.com", "abs.example."},
	} {
		if got := Qualify(tc.owner, tc.zone); got != tc.want {
			t.Errorf("Qualify(%q, %q) = %q, want %q", tc.owner, tc.zone, got, tc.want)
		}
	}
}
