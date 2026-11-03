// SPDX-License-Identifier: AGPL-3.0-only

package feed

import (
	"context"
	"encoding/json"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Publisher keeps the region's input bucket equal to the objects this region
// wrote itself, and copies them into the federation hub so they reach the
// project API. Copies in the hub carry no replicated label, so the hub never
// places them in a region: the other regions get them from the feed.
type Publisher struct {
	Region string
	Local  client.Client
	KV     KV
	// Hub, when set, receives a copy of every local-origin object.
	Hub client.Client
}

// Sync publishes, updates or withdraws one object.
func (p *Publisher) Sync(ctx context.Context, gvk schema.GroupVersionKind, key types.NamespacedName) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	err := p.Local.Get(ctx, key, obj)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	feedKey := Key(gvk.Kind, key.Namespace, key.Name)
	if apierrors.IsNotFound(err) || !obj.GetDeletionTimestamp().IsZero() || obj.GetLabels()[OriginLabel] != p.Region {
		if err := remove(ctx, p.KV, feedKey); err != nil {
			return err
		}
		return p.withdrawFromHub(ctx, gvk, key)
	}
	entry, err := entryOf(obj)
	if err != nil {
		return err
	}
	value, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if err := put(ctx, p.KV, feedKey, value); err != nil {
		return err
	}
	return p.copyToHub(ctx, gvk, obj)
}

// Resync withdraws every key whose object is gone and publishes every
// local-origin object, for events missed while the publisher was down.
func (p *Publisher) Resync(ctx context.Context) error {
	present := map[string]bool{}
	for _, gvk := range Kinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := p.Local.List(ctx, list, client.MatchingLabels{OriginLabel: p.Region}); err != nil {
			return err
		}
		for i := range list.Items {
			o := &list.Items[i]
			present[Key(gvk.Kind, o.GetNamespace(), o.GetName())] = true
			if err := p.Sync(ctx, gvk, types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}); err != nil {
				return err
			}
		}
	}
	keys, err := snapshot(ctx, p.KV)
	if err != nil {
		return err
	}
	for key := range keys {
		if present[key] {
			continue
		}
		kind, namespace, name, ok := parseKey(key)
		gvk, known := known(kind)
		if !ok || !known {
			continue
		}
		if err := p.Sync(ctx, gvk, types.NamespacedName{Namespace: namespace, Name: name}); err != nil {
			return err
		}
	}
	return nil
}

func (p *Publisher) copyToHub(ctx context.Context, gvk schema.GroupVersionKind, obj *unstructured.Unstructured) error {
	if p.Hub == nil {
		return nil
	}
	hub := &unstructured.Unstructured{}
	hub.SetGroupVersionKind(gvk)
	hub.SetNamespace(obj.GetNamespace())
	hub.SetName(obj.GetName())
	_, err := controllerutil.CreateOrPatch(ctx, p.Hub, hub, func() error {
		if origin := hub.GetLabels()[OriginLabel]; hub.GetResourceVersion() != "" && origin != p.Region {
			return &ownedError{key: client.ObjectKeyFromObject(obj), origin: origin}
		}
		labels := map[string]string{}
		for k, v := range obj.GetLabels() {
			labels[k] = v
		}
		labels[OriginLabel] = p.Region
		hub.SetLabels(labels)
		annotations := map[string]string{}
		for k, v := range obj.GetAnnotations() {
			annotations[k] = v
		}
		annotations[OriginGenerationAnnotation] = strconv.FormatInt(obj.GetGeneration(), 10)
		hub.SetAnnotations(annotations)
		hub.Object["spec"] = obj.DeepCopy().Object["spec"]
		return nil
	})
	return err
}

func (p *Publisher) withdrawFromHub(ctx context.Context, gvk schema.GroupVersionKind, key types.NamespacedName) error {
	if p.Hub == nil {
		return nil
	}
	hub := &unstructured.Unstructured{}
	hub.SetGroupVersionKind(gvk)
	if err := p.Hub.Get(ctx, key, hub); err != nil {
		return client.IgnoreNotFound(err)
	}
	if hub.GetLabels()[OriginLabel] != p.Region {
		return nil
	}
	return client.IgnoreNotFound(p.Hub.Delete(ctx, hub))
}

type ownedError struct {
	key    client.ObjectKey
	origin string
}

func (e *ownedError) Error() string {
	return "object " + e.key.String() + " is owned by origin " + strconv.Quote(e.origin) + "; not replacing it"
}

// snapshot reads every key the bucket holds now.
func snapshot(ctx context.Context, kv KV) (map[string]bool, error) {
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := kv.Watch(watchCtx)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for event := range events {
		if event.Synced {
			return keys, nil
		}
		if event.Value == nil {
			delete(keys, event.Key)
		} else {
			keys[event.Key] = true
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errWatchEnded
}
