package controller

import (
	"context"
	"strings"

	downstreamclient "go.miloapis.com/dns-operator/internal/downstreamclient"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
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

func newUnstructuredForGVK(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	return obj
}

func typedEnqueueRequestForGVK(gvk schema.GroupVersionKind) mchandler.TypedEventHandlerFunc[client.Object, GVKRequest] {
	return func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[client.Object, GVKRequest] {
		return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []GVKRequest {
			ctrl.LoggerFrom(ctx).Info("enqueue upstream", "gvk", gvk.String(), "cluster", clusterName, "ns", obj.GetNamespace(), "name", obj.GetName())
			return []GVKRequest{{
				GVK: gvk,
				Request: mcreconcile.Request{
					ClusterName: clusterName,
					Request: reconcile.Request{
						NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()},
					},
				},
			}}
		})
	}
}

func typedEnqueueDownstreamGVKRequest(gvk schema.GroupVersionKind) mchandler.TypedEventHandlerFunc[*unstructured.Unstructured, GVKRequest] {
	return func(_ string, _ cluster.Cluster) handler.TypedEventHandler[*unstructured.Unstructured, GVKRequest] {
		return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj *unstructured.Unstructured) []GVKRequest {
			labels := obj.GetLabels()
			if labels == nil {
				return nil
			}
			if labels[downstreamclient.UpstreamOwnerGroupLabel] != gvk.Group ||
				labels[downstreamclient.UpstreamOwnerKindLabel] != gvk.Kind {
				return nil
			}
			clusterLabel := labels[downstreamclient.UpstreamOwnerClusterNameLabel]
			if clusterLabel == "" {
				return nil
			}
			clusterName := strings.TrimPrefix(strings.ReplaceAll(clusterLabel, "_", "/"), "cluster-")

			return []GVKRequest{{
				GVK: gvk,
				Request: mcreconcile.Request{
					ClusterName: clusterName,
					Request: reconcile.Request{
						NamespacedName: types.NamespacedName{
							Namespace: labels[downstreamclient.UpstreamOwnerNamespaceLabel],
							Name:      labels[downstreamclient.UpstreamOwnerNameLabel],
						},
					},
				},
			}}
		})
	}
}
