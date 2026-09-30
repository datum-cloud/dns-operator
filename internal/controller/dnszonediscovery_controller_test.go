// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcsingle "sigs.k8s.io/multicluster-runtime/providers/single"
)

var discoveryControllerRegistered atomic.Bool

const (
	discoveryControllerName = "dnszonediscovery-replicator"
	discoveryWaitTimeout    = 20 * time.Second
	discoveryQuietPeriod    = 2 * time.Second
	discoveryPollInterval   = 100 * time.Millisecond
)

func TestDNSZoneDiscoveryReplicator_Watch(t *testing.T) {
	if !discoveryControllerRegistered.CompareAndSwap(false, true) {
		t.Skip("the multicluster builder cannot skip controller name validation, so the controller registers once per process")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := newFullTestScheme(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cl, err := cluster.New(cfg, func(o *cluster.Options) { o.Scheme = scheme })
	if err != nil {
		t.Fatalf("new cluster: %v", err)
	}
	mgr, err := mcmanager.New(cfg, mcsingle.New("single", cl), manager.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  "0",
		GracefulShutdownTimeout: ptr.To(time.Second),
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if err := (&DNSZoneDiscoveryReplicator{}).SetupWithManager(mgr); err != nil {
		t.Fatalf("setup controller: %v", err)
	}
	go func() { _ = cl.Start(ctx) }()
	go func() { _ = mgr.Start(ctx) }()

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	newDiscovery := func(t *testing.T, name, zoneRef string) *dnsv1alpha1.DNSZoneDiscovery {
		t.Helper()
		dzd := &dnsv1alpha1.DNSZoneDiscovery{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       dnsv1alpha1.DNSZoneDiscoverySpec{DNSZoneRef: corev1.LocalObjectReference{Name: zoneRef}},
		}
		if err := c.Create(ctx, dzd); err != nil {
			t.Fatalf("create discovery: %v", err)
		}
		return dzd
	}

	t.Run("create reaches the controller", func(t *testing.T) {
		dzd := newDiscovery(t, "created", "missing-a")
		waitForAccepted(t, c, dzd, metav1.ConditionFalse, `"missing-a" not found`)
	})

	t.Run("status-only update does not reconcile", func(t *testing.T) {
		dzd := newDiscovery(t, "status-only", "missing-a")
		waitForAccepted(t, c, dzd, metav1.ConditionFalse, `"missing-a" not found`)
		before := waitForQuietReconciles(t)

		generation := dzd.Generation
		base := dzd.DeepCopy()
		apimeta.RemoveStatusCondition(&dzd.Status.Conditions, CondAccepted)
		if err := c.Status().Patch(ctx, dzd, client.MergeFrom(base)); err != nil {
			t.Fatalf("patch status: %v", err)
		}
		if dzd.Generation != generation {
			t.Fatalf("status patch bumped generation from %d to %d", generation, dzd.Generation)
		}

		deadline := time.Now().Add(discoveryQuietPeriod)
		for time.Now().Before(deadline) {
			if got := discoveryReconcileCount(t); got != before {
				t.Fatalf("status-only update reconciled the discovery controller (%v reconciles, want %v)", got, before)
			}
			time.Sleep(discoveryPollInterval)
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(dzd), dzd); err != nil {
			t.Fatalf("get discovery: %v", err)
		}
		if apimeta.FindStatusCondition(dzd.Status.Conditions, CondAccepted) != nil {
			t.Fatalf("Accepted was restored, so the status-only update was reconciled")
		}
	})

	t.Run("spec change reconciles", func(t *testing.T) {
		dzd := newDiscovery(t, "spec-change", "missing-a")
		waitForAccepted(t, c, dzd, metav1.ConditionFalse, `"missing-a" not found`)

		base := dzd.DeepCopy()
		dzd.Spec.DNSZoneRef.Name = "missing-b"
		if err := c.Patch(ctx, dzd, client.MergeFrom(base)); err != nil {
			t.Fatalf("patch spec: %v", err)
		}
		if dzd.Generation == base.Generation {
			t.Fatalf("spec patch did not bump generation")
		}
		waitForAccepted(t, c, dzd, metav1.ConditionFalse, `"missing-b" not found`)
	})

	t.Run("delete reaches the controller", func(t *testing.T) {
		dzd := newDiscovery(t, "deleted", "missing-a")
		waitForAccepted(t, c, dzd, metav1.ConditionFalse, `"missing-a" not found`)
		before := waitForQuietReconciles(t)

		if err := c.Delete(ctx, dzd); err != nil {
			t.Fatalf("delete discovery: %v", err)
		}
		waitFor(t, "a reconcile after delete", func() bool { return discoveryReconcileCount(t) > before })
	})

	t.Run("owner reference write requeues itself", func(t *testing.T) {
		zone := testZone(func(z *dnsv1alpha1.DNSZone) {
			z.Name = "owner-zone"
			z.Generation = 0
			z.Spec.DomainName = "discovery.invalid"
		})
		if err := c.Create(ctx, zone); err != nil {
			t.Fatalf("create zone: %v", err)
		}
		dzd := newDiscovery(t, "owned", zone.Name)
		waitForAccepted(t, c, dzd, metav1.ConditionTrue, "")
		if !metav1.IsControlledBy(dzd, zone) {
			t.Fatalf("discovery is not controlled by its zone: %v", dzd.OwnerReferences)
		}
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(discoveryWaitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(discoveryPollInterval)
	}
}

func waitForAccepted(t *testing.T, c client.Client, dzd *dnsv1alpha1.DNSZoneDiscovery, status metav1.ConditionStatus, message string) {
	t.Helper()
	waitFor(t, "Accepted="+string(status)+" "+message, func() bool {
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(dzd), dzd); err != nil {
			return false
		}
		cond := apimeta.FindStatusCondition(dzd.Status.Conditions, CondAccepted)
		return cond != nil && cond.Status == status && strings.Contains(cond.Message, message)
	})
}

func waitForQuietReconciles(t *testing.T) float64 {
	t.Helper()
	last := discoveryReconcileCount(t)
	quietSince := time.Now()
	deadline := time.Now().Add(discoveryWaitTimeout)
	for time.Since(quietSince) < discoveryQuietPeriod {
		if time.Now().After(deadline) {
			t.Fatalf("discovery controller never went quiet")
		}
		time.Sleep(discoveryPollInterval)
		if got := discoveryReconcileCount(t); got != last {
			last, quietSince = got, time.Now()
		}
	}
	return last
}

func discoveryReconcileCount(t *testing.T) float64 {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	var total float64
	for _, f := range families {
		if f.GetName() != "controller_runtime_reconcile_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "controller" && l.GetValue() == discoveryControllerName {
					total += m.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}
