// SPDX-License-Identifier: AGPL-3.0-only

package usage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

func TestInventoryReporterEmit(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, dnsv1alpha1.AddToScheme(scheme))

	zone := testZone("example.com", "p-abc", "prod-zone", "project-ns", "uid-1")
	aRecords := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "www-a", Namespace: zone.Namespace},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "www"},
				{Name: "api"},
			},
		},
	}
	aaaaRecords := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "www-aaaa", Namespace: zone.Namespace},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: dnsv1alpha1.RRTypeAAAA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "www"}},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone, aRecords, aaaaRecords).Build()
	rec := &recordingRecorder{}
	r := &InventoryReporter{
		Client:   cl,
		Recorder: rec,
		Location: "us-east-1",
		Interval: time.Minute,
	}
	r.emit(context.Background())

	events := rec.snapshot()
	require.Len(t, events, 3)

	var gotZones, gotA, gotAAAA bool
	for _, ev := range events {
		assert.Equal(t, "p-abc", ev.Project.Name)
		assert.Equal(t, "us-east-1", ev.Dimensions[DimLocation])
		switch ev.Meter {
		case MeterZones:
			gotZones = true
			assert.Equal(t, int64(1), ev.Quantity)
		case MeterRecordsActive:
			switch ev.Dimensions[DimRecordType] {
			case "A":
				gotA = true
				assert.Equal(t, int64(2), ev.Quantity)
			case "AAAA":
				gotAAAA = true
				assert.Equal(t, int64(1), ev.Quantity)
			default:
				t.Errorf("unexpected record type %q", ev.Dimensions[DimRecordType])
			}
		default:
			t.Errorf("unexpected meter %q", ev.Meter)
		}
	}
	assert.True(t, gotZones)
	assert.True(t, gotA)
	assert.True(t, gotAAAA)
}

func TestInventoryReporterSkipsUnattributed(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, dnsv1alpha1.AddToScheme(scheme))
	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "z1", Namespace: "ns"},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.com", DNSZoneClassName: "c"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone).Build()
	rec := &recordingRecorder{}
	r := &InventoryReporter{Client: cl, Recorder: rec, Interval: time.Minute}
	r.emit(context.Background())
	assert.Empty(t, rec.snapshot())
}

func TestCollectorNeedLeaderElection(t *testing.T) {
	t.Parallel()
	assert.False(t, (&Collector{}).NeedLeaderElection())
}

func TestInventoryReporterNeedLeaderElection(t *testing.T) {
	t.Parallel()
	assert.True(t, (&InventoryReporter{}).NeedLeaderElection())
}
