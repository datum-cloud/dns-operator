package controller

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

// GVKRequest wraps a multi-cluster reconcile.Request with the GVK we watch
// (kept as-is, but colocated near the reconciler for discoverability).
type GVKRequest struct {
	mcreconcile.Request
	GVK schema.GroupVersionKind
}

func (r GVKRequest) Cluster() string { return r.ClusterName }
func (r GVKRequest) WithCluster(name string) GVKRequest {
	r.ClusterName = name
	return r
}
