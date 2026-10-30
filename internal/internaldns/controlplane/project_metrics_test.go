// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func histogramCount(t *testing.T, metric prometheus.Metric) uint64 {
	t.Helper()
	var value dto.Metric
	if err := metric.Write(&value); err != nil {
		t.Fatal(err)
	}
	return value.GetHistogram().GetSampleCount()
}

func phaseCount(t *testing.T, phase string) uint64 {
	t.Helper()
	return histogramCount(t, passPhaseDuration.WithLabelValues(phase).(prometheus.Metric))
}

func TestProjectPhaseMetricsInBothSchedulingModes(t *testing.T) {
	for _, perZone := range []bool{false, true} {
		name := "whole-project"
		if perZone {
			name = "per-zone"
		}
		t.Run(name, func(t *testing.T) {
			h, zone := newPerZoneHarness(t)
			h.r.Options.PerZoneReconcile = perZone
			beforePass := histogramCount(t, passDuration)
			phases := []string{"list", "contexts", "associations", "references", "bindings", "ownership", "compile"}
			before := make(map[string]uint64)
			for _, phase := range phases {
				before[phase] = phaseCount(t, phase)
			}
			h.run(projectQueueKey)
			if got := histogramCount(t, passDuration) - beforePass; got != 1 {
				t.Fatalf("project pass count = %d, want 1", got)
			}
			for _, phase := range phases {
				if got := phaseCount(t, phase) - before[phase]; got != 1 {
					t.Errorf("phase %q count = %d, want 1", phase, got)
				}
			}
			beforePersist := phaseCount(t, "zone persist")
			h.settle()
			if phaseCount(t, "zone persist") <= beforePersist {
				t.Fatal("successful publication did not observe zone persist")
			}
			beforePass = histogramCount(t, passDuration)
			beforePersist = phaseCount(t, "zone persist")
			beforeLoad := phaseCount(t, "zone load")
			if perZone {
				h.run(zoneQueuePrefix + zone.Name)
			} else {
				h.run(projectQueueKey)
			}
			if phaseCount(t, "zone load") <= beforeLoad {
				t.Fatal("unchanged zone did not record compile attempt")
			}
			if got := phaseCount(t, "zone persist") - beforePersist; got != 0 {
				t.Errorf("unchanged publication recorded %d persist attempts", got)
			}
			wantPass := uint64(1)
			if perZone {
				wantPass = 0
			}
			if got := histogramCount(t, passDuration) - beforePass; got != wantPass {
				t.Errorf("project pass count = %d, want %d", got, wantPass)
			}
		})
	}
}

type failZoneListClient struct {
	client.Client
	err error
}

func (c failZoneListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*dnsv1alpha1.DNSZoneList); ok {
		return c.err
	}
	return c.Client.List(ctx, list, opts...)
}

func TestProjectPhaseMetricsIncludeFailedReachedPhase(t *testing.T) {
	h, _ := newPerZoneHarness(t)
	wantErr := errors.New("list unavailable")
	h.r.Client = failZoneListClient{Client: h.c, err: wantErr}
	beforePass, beforeList, beforeContexts := histogramCount(t, passDuration), phaseCount(t, "list"), phaseCount(t, "contexts")
	_, err := h.r.Reconcile(h.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "project", Name: projectQueueKey}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if got := histogramCount(t, passDuration) - beforePass; got != 1 {
		t.Errorf("failed pass count = %d, want 1", got)
	}
	if got := phaseCount(t, "list") - beforeList; got != 1 {
		t.Errorf("failed list phase count = %d, want 1", got)
	}
	if got := phaseCount(t, "contexts") - beforeContexts; got != 0 {
		t.Errorf("unreached contexts count = %d, want 0", got)
	}
}

func TestZonePhaseMetricsIncludeFailedLoadWithoutLaterSteps(t *testing.T) {
	h, zone := newPerZoneHarness(t)
	own := &dnsv1alpha1.DNSPublicationOwnership{}
	own.Spec.ActiveManifestName = "missing-manifest"
	beforePass := histogramCount(t, passDuration)
	beforeLoad, beforeConfirm, beforePersist := phaseCount(t, "zone load"), phaseCount(t, "zone confirm"), phaseCount(t, "zone persist")
	_, err := h.r.compileZone(h.ctx, zone, own, nil, nil, nil, nil, nil, h.r.Options.Now())
	if err == nil {
		t.Fatal("missing active manifest must fail compilation")
	}
	if got := phaseCount(t, "zone load") - beforeLoad; got != 1 {
		t.Errorf("failed load count = %d, want 1", got)
	}
	if got := phaseCount(t, "zone confirm") - beforeConfirm; got != 0 {
		t.Errorf("unreached confirmation count = %d, want 0", got)
	}
	if got := phaseCount(t, "zone persist") - beforePersist; got != 0 {
		t.Errorf("unreached persistence count = %d, want 0", got)
	}
	if got := histogramCount(t, passDuration) - beforePass; got != 0 {
		t.Errorf("zone attempt added %d project passes", got)
	}
}
