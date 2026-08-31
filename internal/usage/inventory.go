// SPDX-License-Identifier: AGPL-3.0-only

package usage

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"go.miloapis.com/billing/emission"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

var (
	_ manager.Runnable               = &InventoryReporter{}
	_ manager.LeaderElectionRunnable = &InventoryReporter{}
)

type recordKey struct {
	namespace string
	zoneName  string
}

// InventoryReporter emits hosted-zone and record-inventory gauges from
// downstream shadow objects. Added as a manager Runnable so it only runs
// on the elected replicator leader.
//
// records/active is counted from spec.records (the same sum as
// Status.RecordCount), split by spec.recordType. The CRD comment that
// describes RecordCount as the number of DNSRecordSet resources is stale.
type InventoryReporter struct {
	Client   client.Client
	Recorder emission.Recorder
	Location string
	Interval time.Duration
}

// NeedLeaderElection reports that inventory gauges must run on a single replica.
func (r *InventoryReporter) NeedLeaderElection() bool { return true }

// Start emits inventory gauges immediately and then on Interval until ctx ends.
func (r *InventoryReporter) Start(ctx context.Context) error {
	if r.Interval <= 0 {
		r.Interval = 60 * time.Second
	}
	if r.Recorder == nil {
		r.Recorder = emission.NoopRecorder{}
	}
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	r.emit(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.emit(ctx)
		}
	}
}

func (r *InventoryReporter) emit(ctx context.Context) {
	logger := log.FromContext(ctx)
	var zones dnsv1alpha1.DNSZoneList
	if err := r.Client.List(ctx, &zones); err != nil {
		logger.Error(err, "listing dnszones for usage inventory")
		return
	}
	var recordsets dnsv1alpha1.DNSRecordSetList
	if err := r.Client.List(ctx, &recordsets); err != nil {
		logger.Error(err, "listing dnsrecordsets for usage inventory")
		return
	}

	counts := make(map[recordKey]map[string]int64)
	for i := range recordsets.Items {
		rs := &recordsets.Items[i]
		if rs.DeletionTimestamp != nil {
			continue
		}
		k := recordKey{namespace: rs.Namespace, zoneName: rs.Spec.DNSZoneRef.Name}
		if counts[k] == nil {
			counts[k] = make(map[string]int64)
		}
		rt := string(rs.Spec.RecordType)
		counts[k][rt] += int64(len(rs.Spec.Records))
	}

	now := time.Now()
	for i := range zones.Items {
		zone := &zones.Items[i]
		if zone.DeletionTimestamp != nil {
			continue
		}
		id, ok := IdentityFromZone(zone)
		if !ok {
			continue
		}
		ev := eventForZone(MeterZones, id, 1, r.Location, nil, now)
		if err := r.Recorder.Record(ctx, ev); err != nil {
			logger.Error(err, "recording hosted zone usage", "zone", zone.Name)
		}
		byType := counts[recordKey{namespace: zone.Namespace, zoneName: zone.Name}]
		for rt, n := range byType {
			if n <= 0 {
				continue
			}
			ev := eventForZone(MeterRecordsActive, id, n, r.Location, map[string]string{
				DimRecordType: rt,
			}, now)
			if err := r.Recorder.Record(ctx, ev); err != nil {
				logger.Error(err, "recording record inventory usage", "zone", zone.Name, "recordType", rt)
			}
		}
	}
}
