// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"strconv"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// originUIDAnnotation and originGenerationAnnotation are the UID and
// generation an object has where it was written. A federation hub copy gets
// a UID and generation of its own, but references between objects carry the
// origin's.
const (
	originUIDAnnotation        = "internal-dns.miloapis.com/origin-uid"
	originGenerationAnnotation = "internal-dns.miloapis.com/origin-generation"
	originLabel                = "internal-dns.miloapis.com/origin"
	originProject              = "project"
)

// originUIDClient presents every copy it reads with its origin's UID and
// generation, so references between copies resolve as they do at the origin
// and conditions report the origin's generation. Writes use patches, which
// never carry either.
type originUIDClient struct{ client.Client }

func (c originUIDClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	withOriginUID(obj)
	return nil
}

// List also drops another region's copy that claims the UID of an object
// the project API or this cluster wrote. References resolve by UID, so a
// copy that shares one could stand in for an object it did not write. When
// only other regions' copies share a UID, none of them can be trusted, and
// all are dropped.
func (c originUIDClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := c.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return err
	}
	trusted := map[types.UID]bool{}
	count := map[types.UID]int{}
	for _, o := range items {
		obj, ok := o.(client.Object)
		if !ok {
			continue
		}
		peer := peerCopy(obj)
		withOriginUID(obj)
		count[obj.GetUID()]++
		if !peer {
			trusted[obj.GetUID()] = true
		}
	}
	kept := items[:0]
	for _, o := range items {
		if obj, ok := o.(client.Object); ok && count[obj.GetUID()] > 1 && peerCopy(obj) {
			log.FromContext(ctx).Info("ignoring another region's copy that shares its UID with another object", "namespace", obj.GetNamespace(), "name", obj.GetName(), "uid", obj.GetUID(), "trustedHolder", trusted[obj.GetUID()])
			continue
		}
		kept = append(kept, o)
	}
	return meta.SetList(list, kept)
}

// peerCopy reports whether obj is another region's copy: it carries an origin
// UID but was not written by the project API.
func peerCopy(obj client.Object) bool {
	return obj.GetAnnotations()[originUIDAnnotation] != "" && obj.GetLabels()[originLabel] != originProject
}

func withOriginUID(obj client.Object) {
	if uid := obj.GetAnnotations()[originUIDAnnotation]; uid != "" {
		obj.SetUID(types.UID(uid))
	}
	if g, err := strconv.ParseInt(obj.GetAnnotations()[originGenerationAnnotation], 10, 64); err == nil {
		obj.SetGeneration(g)
	}
}
