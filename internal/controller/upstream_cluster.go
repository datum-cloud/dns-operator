// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

const unregisteredClusterRequeueAfter = 10 * time.Second

type syncedProvider interface {
	HasSynced() bool
}

func getClusterErrorResult(ctx context.Context, mgr mcmanager.Manager, err error) (ctrl.Result, error) {
	if !errors.Is(err, multicluster.ErrClusterNotFound) {
		return ctrl.Result{}, err
	}
	provider, ok := mgr.GetProvider().(syncedProvider)
	if !ok || provider.HasSynced() {
		return ctrl.Result{}, err
	}
	log.FromContext(ctx).V(1).Info("upstream cluster not registered yet, requeueing",
		"requeueAfter", unregisteredClusterRequeueAfter, "error", err.Error())
	return ctrl.Result{RequeueAfter: unregisteredClusterRequeueAfter}, nil
}
