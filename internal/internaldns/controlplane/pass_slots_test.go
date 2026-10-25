// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestProjectPassWaitsForASlot(t *testing.T) {
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	r := &Reconciler{Options: ReconcilerOptions{ProjectUID: "project-a", ProjectNamespace: "project", PassSlots: slots, LeaseDuration: time.Minute}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.Reconcile(ctx, ctrl.Request{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a pass started without a free slot: %v", err)
	}
	if len(slots) != 1 {
		t.Fatalf("a pass that never started changed the slots: %d in use", len(slots))
	}
}

func TestProjectPassReleasesItsSlotAndStopsWaitingAfterAQuarterLease(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := addReferenceStateToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	slots := make(chan struct{}, 1)
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme, Options: ReconcilerOptions{
		ProjectUID: "project-a", ProjectNamespace: "project", PlatformNamespace: "platform", PassSlots: slots, LeaseDuration: 40 * time.Millisecond, AddressAllocator: testAllocator{},
	}}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	if len(slots) != 0 {
		t.Fatalf("a finished pass kept its slot: %d in use", len(slots))
	}
	slots <- struct{}{}
	started := time.Now()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(started); waited < 10*time.Millisecond {
		t.Fatalf("pass did not wait for a slot first: %s", waited)
	}
}
