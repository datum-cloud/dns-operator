package downstreamclient

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// ResourceStrategy is an interface that is used to reduce the burden of
// writing controllers that need to write to downstream resources that are
// artifacts of upstream resources and may need to be placed in downstream
// clusters.
//
// One implementation could just return the client from the cluster that was
// passed in. Another could return a client that ends up rewriting namespaces
// in a way that you can target a single API server and not have conflicts.
// Another could return a client that aligns each source cluster with a target
// cluster, which could be a whole API server, or something like a KCP
// workspace, and doesn't do any namespace/name rewriting.
//
// This way, the controller can be written as if it's putting resources into
// the same namespace as the upstream resource, but that doesn't mean it'll
// land in the same place as that resource.
type ResourceStrategy interface {
	GetClient() client.Client

	// ObjectMetaFromUpstreamObject returns an ObjectMeta struct with Namespace and
	// Name fields populated for the downstream resource.
	ObjectMetaFromUpstreamObject(context.Context, metav1.Object) (metav1.ObjectMeta, error)

	SetControllerReference(context.Context, metav1.Object, metav1.Object, ...controllerutil.OwnerReferenceOption) error
	SetOwnerReference(context.Context, metav1.Object, metav1.Object, ...controllerutil.OwnerReferenceOption) error
	DeleteAnchorForObject(ctx context.Context, owner client.Object) error
}
