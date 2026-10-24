// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type leaseCounts struct {
	takeovers                         float64
	held, created, takenOver, standby float64
	remainingObservations             uint64
}

func readLeaseCounts(t *testing.T) leaseCounts {
	t.Helper()
	var m dto.Metric
	if err := zoneLeaseRemaining.Write(&m); err != nil {
		t.Fatal(err)
	}
	return leaseCounts{
		takeovers:             testutil.ToFloat64(zoneLeaseTakeovers.WithLabelValues("expired")),
		held:                  testutil.ToFloat64(zoneLeaseAcquisitions.WithLabelValues("held")),
		created:               testutil.ToFloat64(zoneLeaseAcquisitions.WithLabelValues("created")),
		takenOver:             testutil.ToFloat64(zoneLeaseAcquisitions.WithLabelValues("taken_over")),
		standby:               testutil.ToFloat64(zoneLeaseAcquisitions.WithLabelValues("standby")),
		remainingObservations: m.GetHistogram().GetSampleCount(),
	}
}

func TestZoneLeaseMetricsCountTakeoversNotRenewals(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
		t.Fatal(err)
	}
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: "project", UID: "zone-metrics"}}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	lease := 30 * time.Second
	writerA := &Reconciler{Client: c, Options: ReconcilerOptions{PlatformNamespace: "platform", Identity: "writer-a", LeaseDuration: lease}}
	writerB := &Reconciler{Client: c, Options: ReconcilerOptions{PlatformNamespace: "platform", Identity: "writer-b", LeaseDuration: lease}}

	before := readLeaseCounts(t)
	if _, _, err := writerA.acquireOwnership(ctx, zone, now); err != nil {
		t.Fatal(err)
	}
	if own, _, err := writerA.acquireOwnership(ctx, zone, now.Add(time.Second)); err != nil || own == nil {
		t.Fatalf("holder did not keep its lease: owner=%v err=%v", own, err)
	}
	if own, _, err := writerB.acquireOwnership(ctx, zone, now.Add(2*time.Second)); err != nil || own != nil {
		t.Fatalf("standby took a live lease: owner=%v err=%v", own, err)
	}
	after := readLeaseCounts(t)
	if got := after.created - before.created; got != 1 {
		t.Fatalf("created delta = %v, want 1", got)
	}
	if got := after.held - before.held; got != 1 {
		t.Fatalf("held delta = %v, want 1", got)
	}
	if got := after.standby - before.standby; got != 1 {
		t.Fatalf("standby delta = %v, want 1", got)
	}
	if got := after.remainingObservations - before.remainingObservations; got != 1 {
		t.Fatalf("remaining observations delta = %v, want 1", got)
	}
	if after.takeovers != before.takeovers || after.takenOver != before.takenOver {
		t.Fatalf("creation, renewal or standby counted as a takeover: before=%+v after=%+v", before, after)
	}

	before = after
	if own, _, err := writerB.acquireOwnership(ctx, zone, now.Add(lease+time.Second)); err != nil || own == nil {
		t.Fatalf("takeover of an expired lease failed: owner=%v err=%v", own, err)
	}
	after = readLeaseCounts(t)
	if got := after.takeovers - before.takeovers; got != 1 {
		t.Fatalf("takeovers delta = %v, want 1", got)
	}
	if got := after.takenOver - before.takenOver; got != 1 {
		t.Fatalf("taken_over delta = %v, want 1", got)
	}

	before = after
	if own, _, err := writerB.acquireOwnership(ctx, zone, now.Add(lease+2*time.Second)); err != nil || own == nil {
		t.Fatalf("new holder did not keep its lease: owner=%v err=%v", own, err)
	}
	after = readLeaseCounts(t)
	if after.takeovers != before.takeovers {
		t.Fatalf("new holder's renewal counted as a takeover: before=%v after=%v", before.takeovers, after.takeovers)
	}
	if got := after.held - before.held; got != 1 {
		t.Fatalf("held delta = %v, want 1", got)
	}
}
