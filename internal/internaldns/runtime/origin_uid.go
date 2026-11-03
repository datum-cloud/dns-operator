// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// originUIDAnnotation is the UID an object has where it was written. A
// federation hub copy gets a UID of its own, but references between objects
// carry the origin's UIDs.
const originUIDAnnotation = "internal-dns.miloapis.com/origin-uid"

// originUIDClient presents every copy it reads with its origin's UID, so
// references between copies resolve as they do at the origin. Writes use
// patches, which never carry the UID.
type originUIDClient struct{ client.Client }

func (c originUIDClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	withOriginUID(obj)
	return nil
}

func (c originUIDClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	return meta.EachListItem(list, func(o k8sruntime.Object) error {
		if obj, ok := o.(client.Object); ok {
			withOriginUID(obj)
		}
		return nil
	})
}

func withOriginUID(obj client.Object) {
	if uid := obj.GetAnnotations()[originUIDAnnotation]; uid != "" {
		obj.SetUID(types.UID(uid))
	}
}
