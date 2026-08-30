// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"context"
	"strings"

	authv1 "k8s.io/api/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
)

const (
	// ParentNameExtraKey is set by Milo on project-scoped admission requests.
	ParentNameExtraKey = "iam.miloapis.com/parent-name"
	// ParentTypeExtraKey is set by Milo to identify the parent resource kind.
	ParentTypeExtraKey = "iam.miloapis.com/parent-type"
)

// clusterNameFromExtra extracts the project control plane cluster name from
// UserInfo.Extra. Only Project parents are treated as cluster context.
func clusterNameFromExtra(extra map[string]authv1.ExtraValue) string {
	if parentTypes, ok := extra[ParentTypeExtraKey]; !ok || len(parentTypes) == 0 || parentTypes[0] != "Project" {
		return ""
	}
	if parentNames, ok := extra[ParentNameExtraKey]; ok && len(parentNames) > 0 && parentNames[0] != "" {
		return parentNames[0]
	}
	return ""
}

// clusterClient returns the project control plane client when cluster context
// is available, otherwise local.
//
// milo v0.7.4 engages cluster-scoped Projects as req.String() ("/projectname"),
// while admission Extra carries the bare project name. Try both forms.
func clusterClient(ctx context.Context, mgr mcmanager.Manager, local client.Client) client.Client {
	if mgr == nil {
		return local
	}
	clusterName, ok := mccontext.ClusterFrom(ctx)
	if !ok || clusterName == "" {
		return local
	}

	cl, err := mgr.GetCluster(ctx, clusterName)
	if err != nil && !strings.HasPrefix(clusterName.String(), "/") {
		cl, err = mgr.GetCluster(ctx, "/"+clusterName)
	}
	if err != nil {
		logf.FromContext(ctx).V(1).Info("falling back to local client for cluster-scoped lookup",
			"cluster", clusterName, "error", err)
		return local
	}
	return cl.GetClient()
}
