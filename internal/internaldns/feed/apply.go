// SPDX-License-Identifier: AGPL-3.0-only

package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/transport"
	corev1 "k8s.io/api/core/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var errWatchEnded = errors.New("feed watch ended")

const (
	replicatedLabel      = "internal-dns.miloapis.com/replicated"
	projectUIDAnnotation = "internal-dns.miloapis.com/project-uid"
	replicatedValue      = "true"
)

var accessBindingList = schema.GroupVersionKind{Group: group, Version: version, Kind: "DNSResolverAccessBindingList"}

// Applier keeps a local copy of every object another region wrote, as that
// region's mirrored input bucket holds them. Copies carry the writer's region
// as their origin, and the applier never replaces an object of another origin.
type Applier struct {
	// Region is the region that wrote the mirrored bucket.
	Region string
	Local  client.Client
	Mirror KV
	// CaughtUp reports whether the mirror holds everything its source does.
	// Copies whose key is missing are removed only then, so a new or
	// lagging mirror never withdraws what the writer still holds.
	CaughtUp func(context.Context) (bool, error)
	// Interval paces retries of failed keys and the caught-up check.
	Interval time.Duration

	served   map[string]bool
	servedAt time.Time
}

// Run applies the mirror until ctx ends. Keys that fail are retried, and once
// the mirror has replayed and caught up, copies whose key is gone are removed.
func (a *Applier) Run(ctx context.Context) error {
	events, err := a.Mirror.Watch(ctx)
	if err != nil {
		return err
	}
	interval := a.Interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	present := map[string]bool{}
	failed := map[string]bool{}
	synced, pruned := false, false
	apply := func(key string, value []byte) {
		err := a.Apply(ctx, key, value)
		var refused *refusedError
		switch {
		case errors.As(err, &refused):
			// Another origin holds the key or the identity: retrying cannot
			// change that, so it waits for the next write of the key.
			delete(failed, key)
			log.FromContext(ctx).Error(err, "refused another region's input", "region", a.Region, "key", key)
		case err != nil:
			failed[key] = true
			log.FromContext(ctx).Error(err, "apply another region's input; retrying", "region", a.Region, "key", key)
		default:
			delete(failed, key)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-events:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return errWatchEnded
			}
			if event.Synced {
				synced = true
				continue
			}
			if event.Value == nil {
				delete(present, event.Key)
			} else {
				present[event.Key] = true
			}
			apply(event.Key, event.Value)
		case <-ticker.C:
			for key := range failed {
				value, _, err := a.Mirror.Get(ctx, key)
				if errors.Is(err, transport.ErrKVNotFound) {
					value, err = nil, nil
				}
				if err != nil {
					continue
				}
				apply(key, value)
			}
			if synced && !pruned && a.CaughtUp != nil {
				ready, err := a.CaughtUp(ctx)
				if err != nil || !ready {
					continue
				}
				if err := a.prune(ctx, present); err != nil {
					log.FromContext(ctx).Error(err, "remove copies the writing region withdrew", "region", a.Region)
					continue
				}
				pruned = true
			}
			if pruned {
				if err := a.withdrawUnserved(ctx); err != nil {
					log.FromContext(ctx).Error(err, "remove copies in projects the writing region no longer serves", "region", a.Region)
				}
			}
		}
	}
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
		return &refusedError{fmt.Sprintf("%s/%s %s is owned by origin %q, not %q; not replacing it", namespace, name, kind, local.GetLabels()[OriginLabel], a.Region)}
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
	if entry.Namespace != namespace || entry.Name != name {
		return fmt.Errorf("%s names %s/%s", key, entry.Namespace, entry.Name)
	}
	if err := a.mapped(ctx, namespace); err != nil {
		return err
	}
	if err := a.uniqueIdentity(ctx, gvk, namespace, name, entry.Annotations[OriginUIDAnnotation]); err != nil {
		return err
	}
	if exists && local.GetAnnotations()[OriginUIDAnnotation] != entry.Annotations[OriginUIDAnnotation] {
		// The writer deleted and recreated the object: replace the copy so it
		// carries the new identity.
		if err := a.Local.Delete(ctx, local); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return fmt.Errorf("replaced %s/%s %s after the writer recreated it; applying again", namespace, name, kind)
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
			if !reserved(k) {
				labels[k] = v
			}
		}
		labels[OriginLabel] = a.Region
		local.SetLabels(labels)
		annotations := map[string]string{}
		for k, v := range entry.Annotations {
			if !reserved(k) {
				annotations[k] = v
			}
		}
		annotations[OriginGenerationAnnotation] = strconv.FormatInt(entry.Generation, 10)
		annotations[OriginUIDAnnotation] = entry.Annotations[OriginUIDAnnotation]
		local.SetAnnotations(annotations)
		local.Object["spec"] = spec
		return nil
	})
	return err
}

// refusedError is an entry the applier will never accept as it stands.
type refusedError struct{ msg string }

func (e *refusedError) Error() string { return e.msg }

// uniqueIdentity refuses an entry whose origin UID another object of its kind
// in the namespace already answers to. References resolve by that UID, so a
// peer claiming one could stand in for an object it did not write.
func (a *Applier) uniqueIdentity(ctx context.Context, gvk schema.GroupVersionKind, namespace, name, uid string) error {
	if len(uid) != 36 || strings.Count(uid, "-") != 4 {
		return &refusedError{fmt.Sprintf("%s/%s %s has no well-formed origin UID", namespace, name, gvk.Kind)}
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := a.Local.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return err
	}
	for i := range list.Items {
		o := &list.Items[i]
		if o.GetName() == name {
			continue
		}
		held := o.GetAnnotations()[OriginUIDAnnotation]
		if held == "" {
			held = string(o.GetUID())
		}
		if held == uid {
			return &refusedError{fmt.Sprintf("%s/%s %s claims the UID of %s", namespace, name, gvk.Kind, o.GetName())}
		}
	}
	return nil
}

// mapped accepts only namespaces the hub placed here for a project that the
// writing region serves, so a peer cannot write anywhere else in this
// cluster, nor into a project it has no network in. A project the region
// does not serve yet is retried: its access may still be on its way here.
func (a *Applier) mapped(ctx context.Context, namespace string) error {
	var ns corev1.Namespace
	if err := a.Local.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		return fmt.Errorf("namespace %s: %w", namespace, err)
	}
	project := ns.Annotations[projectUIDAnnotation]
	if ns.Labels[replicatedLabel] != replicatedValue || !ns.DeletionTimestamp.IsZero() || project == "" {
		return fmt.Errorf("namespace %s is not a live project namespace", namespace)
	}
	served, err := a.servedProjects(ctx)
	if err != nil {
		return err
	}
	if !served[project] {
		return fmt.Errorf("region %s has no resolver access in project %s; not accepting its inputs for namespace %s", a.Region, project, namespace)
	}
	return nil
}

// servedProjects are the projects with resolver access in the writing
// region, by project UID. They are read again at most once per interval.
func (a *Applier) servedProjects(ctx context.Context) (map[string]bool, error) {
	interval := a.Interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	if a.served != nil && time.Since(a.servedAt) < interval {
		return a.served, nil
	}
	var namespaces corev1.NamespaceList
	if err := a.Local.List(ctx, &namespaces, client.MatchingLabels{replicatedLabel: replicatedValue}); err != nil {
		return nil, err
	}
	projectOf := map[string]string{}
	for _, ns := range namespaces.Items {
		if uid := ns.Annotations[projectUIDAnnotation]; uid != "" && ns.DeletionTimestamp.IsZero() {
			projectOf[ns.Name] = uid
		}
	}
	bindings := &unstructured.UnstructuredList{}
	bindings.SetGroupVersionKind(accessBindingList)
	if err := a.Local.List(ctx, bindings); err != nil {
		return nil, err
	}
	served := map[string]bool{}
	for i := range bindings.Items {
		b := &bindings.Items[i]
		region, _, _ := unstructured.NestedString(b.Object, "spec", "region")
		if project := projectOf[b.GetNamespace()]; region == a.Region && project != "" {
			served[project] = true
		}
	}
	a.served, a.servedAt = served, time.Now()
	return served, nil
}

// reserved keys are the ones replication and the hub act on. A peer's entry
// cannot set them; the applier writes its own.
func reserved(key string) bool {
	prefix, _, ok := strings.Cut(key, "/")
	return ok && (prefix == "internal-dns.miloapis.com" || prefix == "kubectl.kubernetes.io" || prefix == "karmada.io" || strings.HasSuffix(prefix, ".karmada.io"))
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

// withdrawUnserved removes the writing region's copies in projects that no
// longer have resolver access in that region, so access withdrawn after a
// copy landed does not leave its names in place.
func (a *Applier) withdrawUnserved(ctx context.Context) error {
	served, err := a.servedProjects(ctx)
	if err != nil {
		return err
	}
	var namespaces corev1.NamespaceList
	if err := a.Local.List(ctx, &namespaces, client.MatchingLabels{replicatedLabel: replicatedValue}); err != nil {
		return err
	}
	unserved := map[string]bool{}
	for _, ns := range namespaces.Items {
		if project := ns.Annotations[projectUIDAnnotation]; project != "" && !served[project] {
			unserved[ns.Name] = true
		}
	}
	if len(unserved) == 0 {
		return nil
	}
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
			if !unserved[o.GetNamespace()] {
				continue
			}
			if err := a.Local.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
				errs = errors.Join(errs, err)
			}
		}
	}
	return errs
}
