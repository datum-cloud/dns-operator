// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"testing"

	"go.miloapis.com/dns-operator/internal/internaldns/platform"
)

func TestWhileLeadingSkipsStepsWithoutTheRegionLease(t *testing.T) {
	ran := 0
	step := func(context.Context) error { ran++; return nil }
	for _, lease := range []*platform.RegionLease{nil, {}} {
		if err := whileLeading(step, lease)(context.Background()); err != nil || ran != 0 {
			t.Fatalf("a process without the region lease ran a leader-only step: ran=%d err=%v", ran, err)
		}
	}
}
