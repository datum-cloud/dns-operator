// SPDX-License-Identifier: AGPL-3.0-only

package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var errWatchEnded = errors.New("feed watch ended")

// Applier keeps a local copy of every object another region wrote, as that
// region's mirrored input bucket holds them. Copies carry the writer's region
// as their origin, and the applier never replaces an object of another origin.
type Applier struct {
	// Region is the region that wrote the mirrored bucket.
	Region string
	Local  client.Client
	Mirror KV
}

// Run applies the mirror until ctx ends. Once the mirror's current keys have
// replayed, copies whose key is gone are removed.
func (a *Applier) Run(ctx context.Context) error {
	events, err := a.Mirror.Watch(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	synced := false
	for event := range events {
		if event.Synced {
			if !synced {
				if err := a.prune(ctx, seen); err != nil {
					log.FromContext(ctx).Error(err, "remove copies the writing region withdrew", "region", a.Region)
				}
				synced = true
			}
			continue
		}
		if !synced {
			seen[event.Key] = event.Value != nil
		}
		if err := a.Apply(ctx, event.Key, event.Value); err != nil {
			log.FromContext(ctx).Error(err, "apply another region's input", "region", a.Region, "key", event.Key)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil
	}
	return errWatchEnded
}

// Apply writes or removes the local copy for one key. A nil value is a
// delete.
func (a *Applier) Apply(ctx context.Context, key string, value []byte) error {
	kind, namespace, name, ok := parseKey(key)
	gvk, isKnown := known(kind)
	if !ok || !isKnown {
		return nil
	}
	local := &unstructured.Unstructured{}
	local.SetGroupVersionKind(gvk)
	err := a.Local.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, local)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	exists := err == nil
	if exists && local.GetLabels()[OriginLabel] != a.Region {
		return fmt.Errorf("%s/%s %s is owned by origin %q, not %q; not replacing it", namespace, name, kind, local.GetLabels()[OriginLabel], a.Region)
	}
	if value == nil {
		if !exists {
			return nil
		}
		return client.IgnoreNotFound(a.Local.Delete(ctx, local))
	}
	var entry Entry
	if err := json.Unmarshal(value, &entry); err != nil {
		return fmt.Errorf("decode %s: %w", key, err)
	}
	if exists {
		if g, err := strconv.ParseInt(local.GetAnnotations()[OriginGenerationAnnotation], 10, 64); err == nil && g > entry.Generation {
			return nil
		}
	}
	var spec any
	if err := json.Unmarshal(entry.Spec, &spec); err != nil {
		return fmt.Errorf("decode %s spec: %w", key, err)
	}
	local.SetNamespace(namespace)
	local.SetName(name)
	_, err = controllerutil.CreateOrPatch(ctx, a.Local, local, func() error {
		labels := map[string]string{}
		for k, v := range entry.Labels {
			labels[k] = v
		}
		labels[OriginLabel] = a.Region
		local.SetLabels(labels)
		annotations := map[string]string{}
		for k, v := range entry.Annotations {
			annotations[k] = v
		}
		annotations[OriginGenerationAnnotation] = strconv.FormatInt(entry.Generation, 10)
		local.SetAnnotations(annotations)
		local.Object["spec"] = spec
		return nil
	})
	return err
}

func (a *Applier) prune(ctx context.Context, seen map[string]bool) error {
	var errs error
	for _, gvk := range Kinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := a.Local.List(ctx, list, client.MatchingLabels{OriginLabel: a.Region}); err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		for i := range list.Items {
			o := &list.Items[i]
			if seen[Key(gvk.Kind, o.GetNamespace(), o.GetName())] {
				continue
			}
			if err := a.Local.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
				errs = errors.Join(errs, err)
			}
		}
	}
	return errs
}
