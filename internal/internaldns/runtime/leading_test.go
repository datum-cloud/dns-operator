// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"testing"

	"go.miloapis.com/dns-operator/internal/internaldns/platform"
)

func TestWhileLeadingSkipsStepsOnStandbys(t *testing.T) {
	ran := 0
	step := whileLeading(func(context.Context) error { ran++; return nil }, &platform.Planner{})
	if err := step(context.Background()); err != nil || ran != 0 {
		t.Fatalf("a standby ran a leader-only step: ran=%d err=%v", ran, err)
	}
}
