// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var ownershipResource = schema.GroupResource{Group: "internal-dns.miloapis.com", Resource: "dnspublicationownerships"}

type leaseCounts struct {
	takeovers float64
	acquired  map[string]float64
	renewals  map[string]float64
	remaining map[string]*dto.Histogram
}

func readLeaseCounts(t *testing.T) leaseCounts {
	t.Helper()
	c := leaseCounts{takeovers: testutil.ToFloat64(zoneLeaseTakeovers), acquired: map[string]float64{}, renewals: map[string]float64{}, remaining: map[string]*dto.Histogram{}}
	for _, v := range zoneLeaseAcquisitionResults {
		c.acquired[v] = testutil.ToFloat64(zoneLeaseAcquisitions.WithLabelValues(v))
	}
	for _, v := range zoneLeaseRenewalResults {
		c.renewals[v] = testutil.ToFloat64(zoneLeaseRenewals.WithLabelValues(v))
	}
	for _, v := range zoneLeaseRemainingPoints {
		var m dto.Metric
		if err := zoneLeaseRemaining.WithLabelValues(v).(prometheus.Metric).Write(&m); err != nil {
			t.Fatal(err)
		}
		c.remaining[v] = m.GetHistogram()
	}
	return c
}

func bucketCount(h *dto.Histogram, le float64) uint64 {
	for _, b := range h.GetBucket() {
		if b.GetUpperBound() == le {
			return b.GetCumulativeCount()
		}
	}
	return 0
}

func (before leaseCounts) expect(t *testing.T, takeovers float64, acquired, renewals map[string]float64) leaseCounts {
	t.Helper()
	after := readLeaseCounts(t)
	if got := after.takeovers - before.takeovers; got != takeovers {
		t.Fatalf("takeovers delta = %v, want %v", got, takeovers)
	}
	for _, v := range zoneLeaseAcquisitionResults {
		if got := after.acquired[v] - before.acquired[v]; got != acquired[v] {
			t.Fatalf("acquisitions{result=%q} delta = %v, want %v", v, got, acquired[v])
		}
	}
	for _, v := range zoneLeaseRenewalResults {
		if got := after.renewals[v] - before.renewals[v]; got != renewals[v] {
			t.Fatalf("renewals{result=%q} delta = %v, want %v", v, got, renewals[v])
		}
	}
	return after
}

func leaseFixture(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.WithWatch {
	t.Helper()
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

func leaseOwner(zone *dnsv1alpha1.DNSZone, holder string, until time.Time) *dnsv1alpha1.DNSPublicationOwnership {
	return &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-" + model.OpaqueToken(string(zone.UID))[:20], Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{
		ZoneUID: zone.UID, HolderIdentity: holder, WriterEpoch: 1, NextRevision: 1, LeaseUntil: metav1.NewTime(until),
	}}
}

func leaseWriter(c client.Client, identity string, lease time.Duration, now func() time.Time) *Reconciler {
	return &Reconciler{Client: c, Options: ReconcilerOptions{PlatformNamespace: "platform", Identity: identity, LeaseDuration: lease, Now: now}}
}

func TestZoneLeaseMetricsCountTakeoversNotRenewals(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: "project", UID: "zone-metrics"}}
	c := leaseFixture(t, interceptor.Funcs{})
	lease := 30 * time.Second
	writerA := leaseWriter(c, "writer-a", lease, clock)
	writerB := leaseWriter(c, "writer-b", lease, clock)

	before := readLeaseCounts(t)
	if _, _, err := writerA.acquireOwnership(ctx, zone, now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if own, _, err := writerA.acquireOwnership(ctx, zone, now); err != nil || own == nil {
		t.Fatalf("holder did not keep its lease: owner=%v err=%v", own, err)
	}
	now = now.Add(time.Second)
	if own, _, err := writerB.acquireOwnership(ctx, zone, now); err != nil || own != nil {
		t.Fatalf("standby took a live lease: owner=%v err=%v", own, err)
	}
	before = before.expect(t, 0, map[string]float64{"created": 1, "held": 1, "standby": 1}, nil)
	if got := testutil.ToFloat64(zoneLeaseDuration); got != lease.Seconds() {
		t.Fatalf("lease duration gauge = %v, want %v", got, lease.Seconds())
	}

	now = now.Add(lease)
	if own, _, err := writerB.acquireOwnership(ctx, zone, now); err != nil || own == nil {
		t.Fatalf("takeover of an expired lease failed: owner=%v err=%v", own, err)
	}
	before = before.expect(t, 1, map[string]float64{"taken_over": 1}, nil)

	now = now.Add(time.Second)
	if own, _, err := writerB.acquireOwnership(ctx, zone, now); err != nil || own == nil {
		t.Fatalf("new holder did not keep its lease: owner=%v err=%v", own, err)
	}
	before.expect(t, 0, map[string]float64{"held": 1}, nil)
}

func TestZoneLeaseMetricsCountConflicts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: "project", UID: "zone-conflicts"}}

	createRace := leaseFixture(t, interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
		return apierrors.NewAlreadyExists(ownershipResource, "owner")
	}})
	before := readLeaseCounts(t)
	if own, _, err := leaseWriter(createRace, "writer-a", 30*time.Second, clock).acquireOwnership(ctx, zone, now); err != nil || own != nil {
		t.Fatalf("lost create race returned owner=%v err=%v", own, err)
	}
	before = before.expect(t, 0, map[string]float64{"conflict": 1}, nil)

	patchRace := leaseFixture(t, interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
		return apierrors.NewConflict(ownershipResource, "owner", nil)
	}}, leaseOwner(zone, "writer-a", now.Add(-time.Second)))
	if _, _, err := leaseWriter(patchRace, "writer-b", 30*time.Second, clock).acquireOwnership(ctx, zone, now); !apierrors.IsConflict(err) {
		t.Fatalf("takeover patch conflict returned %v", err)
	}
	before.expect(t, 0, map[string]float64{"conflict": 1}, nil)
}

func TestZoneLeaseRemainingUsesCurrentClock(t *testing.T) {
	ctx := context.Background()
	passStart := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	now := passStart
	clock := func() time.Time { return now }
	lease := 30 * time.Second
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: "project", UID: "zone-clock"}}
	c := leaseFixture(t, interceptor.Funcs{}, leaseOwner(zone, "writer-a", passStart.Add(lease)))
	writer := leaseWriter(c, "writer-a", lease, clock)

	before := readLeaseCounts(t)
	now = passStart.Add(lease - 500*time.Millisecond)
	if own, _, err := writer.acquireOwnership(ctx, zone, passStart); err != nil || own == nil {
		t.Fatalf("holder did not keep its lease: owner=%v err=%v", own, err)
	}
	after := readLeaseCounts(t)
	if got := bucketCount(after.remaining["acquire"], 1) - bucketCount(before.remaining["acquire"], 1); got != 1 {
		t.Fatalf("headroom measured against the pass start, not the current clock: le=1 delta = %d", got)
	}

	before = after
	now = passStart.Add(lease + time.Second)
	if own, _, err := writer.acquireOwnership(ctx, zone, passStart); err != nil || own == nil {
		t.Fatalf("holder did not keep its lapsed lease: owner=%v err=%v", own, err)
	}
	after = readLeaseCounts(t)
	if got := bucketCount(after.remaining["acquire"], 0) - bucketCount(before.remaining["acquire"], 0); got != 1 {
		t.Fatalf("lapsed lease not in the lowest bucket: le=0 delta = %d", got)
	}
}

func TestZoneLeaseRenewalsCountResults(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	lease := 30 * time.Second
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: "project", UID: "zone-renewals"}}
	var patchErr error
	c := leaseFixture(t, interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if patchErr != nil {
			return patchErr
		}
		return cl.Patch(ctx, obj, patch, opts...)
	}}, leaseOwner(zone, "writer-a", now.Add(lease)))
	writer := leaseWriter(c, "writer-a", lease, clock)
	own, _, err := writer.acquireOwnership(ctx, zone, now)
	if err != nil || own == nil {
		t.Fatalf("holder did not keep its lease: owner=%v err=%v", own, err)
	}
	renew := func() error {
		base := own.DeepCopy()
		own.Spec.LeaseUntil = metav1.NewTime(now.Add(lease))
		return writer.patchLease(ctx, own, base)
	}

	before := readLeaseCounts(t)
	now = now.Add(lease / 2)
	if err := renew(); err != nil {
		t.Fatal(err)
	}
	before = before.expect(t, 0, nil, map[string]float64{"renewed": 1})

	now = now.Add(2 * lease)
	patchErr = apierrors.NewConflict(ownershipResource, "owner", nil)
	if err := renew(); !apierrors.IsConflict(err) {
		t.Fatalf("renewal conflict returned %v", err)
	}
	after := before.expect(t, 0, nil, map[string]float64{"conflict": 1})
	if got := bucketCount(after.remaining["renew"], 0) - bucketCount(before.remaining["renew"], 0); got != 1 {
		t.Fatalf("lease lost mid-pass not in the lowest renew bucket: le=0 delta = %d", got)
	}

	patchErr = apierrors.NewServiceUnavailable("down")
	if err := renew(); err == nil {
		t.Fatal("renewal error was swallowed")
	}
	after.expect(t, 0, nil, map[string]float64{"error": 1})
}
