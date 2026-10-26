// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"testing"
	"time"
)

func TestRegionLeaseElectsOneLeader(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 13, 30, 0, 0, time.UTC)
	cl := testClient(t)
	lease := func(identity string) *RegionLease {
		return &RegionLease{Client: cl, Namespace: "platform", Region: "east", Identity: identity, LeaseDuration: time.Minute, Now: func() time.Time { return now }}
	}
	a, b := lease("a"), lease("b")
	if err := a.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if !a.Leads(now) || b.Leads(now) {
		t.Fatalf("want a to lead alone: a=%v b=%v", a.Leads(now), b.Leads(now))
	}

	// A holder that stops renewing loses the region once its lease runs out.
	now = now.Add(2 * time.Minute)
	if err := b.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if !b.Leads(now) {
		t.Fatal("a standby did not take over an expired region lease")
	}
	if err := a.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if a.Leads(now) {
		t.Fatal("the old holder still leads after another process took over")
	}

	// A released lease is taken over at once.
	if ok, err := b.Release(ctx); err != nil || !ok {
		t.Fatalf("release: %v %v", ok, err)
	}
	if b.Leads(now) {
		t.Fatal("a released lease still leads")
	}
	if err := a.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if !a.Leads(now) {
		t.Fatal("a standby did not take over a released region lease")
	}

	stopping := lease("c")
	stopping.Stopping = func() bool { return true }
	now = now.Add(5 * time.Minute)
	if err := stopping.Step(ctx); err != nil || stopping.Leads(now) {
		t.Fatalf("a stopping process took the region lease: %v", err)
	}
}
