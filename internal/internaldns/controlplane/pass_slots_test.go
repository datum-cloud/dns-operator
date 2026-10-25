// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

func TestProjectPassWaitsForASlot(t *testing.T) {
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	r := &Reconciler{Options: ReconcilerOptions{ProjectUID: "project-a", ProjectNamespace: "project", PassSlots: slots}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.Reconcile(ctx, ctrl.Request{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a pass started without a free slot: %v", err)
	}
	if len(slots) != 1 {
		t.Fatalf("a pass that never started changed the slots: %d in use", len(slots))
	}
}
