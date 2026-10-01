// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	milomulticluster "go.miloapis.com/milo/pkg/multicluster-runtime/milo"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

var _ syncedProvider = (*milomulticluster.Provider)(nil)

func TestMiloProviderMeetsContract(t *testing.T) {
	p := &milomulticluster.Provider{}
	if _, err := p.Get(context.Background(), "unregistered"); !errors.Is(err, multicluster.ErrClusterNotFound) {
		t.Fatalf("expected ErrClusterNotFound, got %v", err)
	}
	if p.HasSynced() {
		t.Fatal("expected a provider that has not started to report not synced")
	}
}

type fakeProvider struct {
	multicluster.Provider
}

type fakeSyncingProvider struct {
	fakeProvider
	synced bool
}

func (p *fakeSyncingProvider) HasSynced() bool { return p.synced }

type fakeClusterManager struct {
	mcmanager.Manager
	provider multicluster.Provider
	err      error
}

func (m *fakeClusterManager) GetCluster(context.Context, multicluster.ClusterName) (cluster.Cluster, error) {
	return nil, m.err
}

func (m *fakeClusterManager) GetProvider() multicluster.Provider {
	return m.provider
}

func clusterNotFound(name string) error {
	return fmt.Errorf("cluster %s: %w", name, multicluster.ErrClusterNotFound)
}

type getClusterCase struct {
	name        string
	provider    multicluster.Provider
	err         error
	wantRequeue bool
}

func getClusterCases() []getClusterCase {
	return []getClusterCase{
		{
			name:        "provider not synced and cluster not found requeues",
			provider:    &fakeSyncingProvider{synced: false},
			err:         clusterNotFound("p1"),
			wantRequeue: true,
		},
		{
			name:     "provider synced and cluster not found returns the error",
			provider: &fakeSyncingProvider{synced: true},
			err:      clusterNotFound("p1"),
		},
		{
			name:     "provider without HasSynced returns the error",
			provider: &fakeProvider{},
			err:      clusterNotFound("p1"),
		},
		{
			name:     "no provider returns the error",
			provider: nil,
			err:      clusterNotFound("p1"),
		},
		{
			name:     "other error from an unsynced provider returns the error",
			provider: &fakeSyncingProvider{synced: false},
			err:      errors.New("cluster p1 not found"),
		},
	}
}

func assertGetClusterResult(t *testing.T, tc getClusterCase, res ctrl.Result, err error) {
	t.Helper()
	if tc.wantRequeue {
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if res.RequeueAfter != unregisteredClusterRequeueAfter {
			t.Fatalf("expected RequeueAfter %s, got %s", unregisteredClusterRequeueAfter, res.RequeueAfter)
		}
		return
	}
	if !errors.Is(err, tc.err) {
		t.Fatalf("expected error %v, got %v", tc.err, err)
	}
	if res != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got %+v", res)
	}
}

func TestGetClusterErrorResult(t *testing.T) {
	for _, tc := range getClusterCases() {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &fakeClusterManager{provider: tc.provider}
			res, err := getClusterErrorResult(context.Background(), mgr, tc.err)
			assertGetClusterResult(t, tc, res, err)
		})
	}
}

func TestReconcile_UpstreamClusterNotRegistered(t *testing.T) {
	reconcilers := map[string]func(mcmanager.Manager) mcreconcile.Reconciler{
		"DNSZoneReplicator": func(m mcmanager.Manager) mcreconcile.Reconciler {
			return &DNSZoneReplicator{mgr: m}
		},
		"DNSRecordSetReplicator": func(m mcmanager.Manager) mcreconcile.Reconciler {
			return &DNSRecordSetReplicator{mgr: m}
		},
		"DNSZoneDiscoveryReplicator": func(m mcmanager.Manager) mcreconcile.Reconciler {
			return &DNSZoneDiscoveryReplicator{mgr: m}
		},
	}
	req := mcreconcile.Request{ClusterName: "p1"}
	req.NamespacedName = types.NamespacedName{Namespace: "ns", Name: "obj"}

	for rname, newReconciler := range reconcilers {
		for _, tc := range getClusterCases() {
			t.Run(rname+"/"+tc.name, func(t *testing.T) {
				mgr := &fakeClusterManager{provider: tc.provider, err: tc.err}
				res, err := newReconciler(mgr).Reconcile(context.Background(), req)
				assertGetClusterResult(t, tc, res, err)
			})
		}
	}
}
