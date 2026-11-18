// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

const deletedProject = "p2"

// TestStandbyTakesTheLeaseAfterAProjectIsDeleted requires a standby that takes
// the lease to reconcile its projects after a project it engaged is deleted.
func TestStandbyTakesTheLeaseAfterAProjectIsDeleted(t *testing.T) {
	// The Milo provider registers a process-wide controller name, and
	// TestNonLeaderReplica starts one, so this test runs in a process of its own.
	if os.Getenv(ownProcessEnv) != t.Name() {
		runInOwnProcess(t)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := startPlanes(t)
	deploymentClient := newClient(t, p.deployment)
	createReadyProject(ctx, t, deploymentClient, deletedProject)
	downstream := newCluster(t, p.deployment)
	mcmgr := startReplica(ctx, t, p, true, downstream, ctrl.Options{}, zoneReplicator(downstream))

	for _, name := range []string{testProject, deletedProject} {
		if err := pollMinute(ctx, engaged(mcmgr, name, true)); err != nil {
			t.Fatalf("the standby never engaged project %s: %v", name, err)
		}
	}
	if err := deploymentClient.Delete(ctx, projectObject(deletedProject)); err != nil {
		t.Fatalf("deleting project %s: %v", deletedProject, err)
	}
	if err := pollMinute(ctx, engaged(mcmgr, deletedProject, false)); err != nil {
		t.Fatalf("the standby never disengaged the deleted project %s: %v", deletedProject, err)
	}

	requireNotLeader(t, mcmgr)
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: replicatorLeaseName, Namespace: testNamespace}}
	if err := deploymentClient.Delete(ctx, lease); err != nil {
		t.Fatalf("deleting the held lease: %v", err)
	}
	select {
	case <-mcmgr.Elected():
	case <-time.After(30 * time.Second):
		t.Fatal("the standby did not take the free lease within 30 s")
	}
	if !finalizedWithin(ctx, t, newClient(t, p.project), 60*time.Second) {
		t.Fatal("the new leader never reconciled the DNSZone of the project that remains: " +
			"its controller did not start, so the manager stopped")
	}
}

// engaged reports whether the replica's provider holds the named project's
// cluster, when want is true, or no longer holds it, when want is false.
func engaged(mcmgr mcmanager.Manager, name string, want bool) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		_, err := mcmgr.GetCluster(ctx, multicluster.ClusterName(name))
		if err != nil && !errors.Is(err, multicluster.ErrClusterNotFound) {
			return false, err
		}
		return (err == nil) == want, nil
	}
}

// pollMinute checks condition every second until it holds, for a minute.
func pollMinute(ctx context.Context, condition wait.ConditionWithContextFunc) error {
	return wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, condition)
}
