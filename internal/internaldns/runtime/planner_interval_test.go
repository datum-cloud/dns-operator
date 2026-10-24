// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"testing"
	"time"
)

func TestPlannerIntervalIsCappedAndFloored(t *testing.T) {
	for lease, want := range map[time.Duration]time.Duration{
		120 * time.Second: 10 * time.Second,
		30 * time.Second:  10 * time.Second,
		15 * time.Second:  5 * time.Second,
		2 * time.Second:   time.Second,
		0:                 time.Second,
	} {
		if got := plannerInterval(lease); got != want {
			t.Errorf("plannerInterval(%s) = %s, want %s", lease, got, want)
		}
	}
}
