// SPDX-License-Identifier: AGPL-3.0-only

package privatereplication

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

// Replicator copies one private DNS kind from every project into the hub.
// Objects a region wrote itself carry that region as their origin and are not
// copied: the region already has them and sends them to the others itself.
type Replicator struct {
	GVK schema.GroupVersionKind
	// Hub is a client for the federation hub's API.
	Hub ctrl.Client
	// ProjectUID resolves a project's UID from its cluster name.
	ProjectUID func(context.Context, multicluster.ClusterName) (types.UID, error)

	mgr mcmanager.Manager
}

func (r *Replicator) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.GVK)
	return mcbuilder.ControllerManagedBy(mgr).
		Named("private-replicator-" + strings.ToLower(r.GVK.Kind)).
		For(obj).
		Complete(r)
}

func (r *Replicator) Reconcile(ctx context.Context, req mcreconcile.Request) (reconcile.Result, error) {
	cl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, r.replicate(ctx, cl.GetClient(), req.ClusterName, req.NamespacedName)
}

func (r *Replicator) replicate(ctx context.Context, project ctrl.Client, clusterName multicluster.ClusterName, key types.NamespacedName) error {
	up := &unstructured.Unstructured{}
	up.SetGroupVersionKind(r.GVK)
	if err := project.Get(ctx, key, up); err != nil {
		return ctrl.IgnoreNotFound(err)
	}
	if origin := up.GetLabels()[OriginLabel]; origin != "" && origin != OriginProject {
		return nil
	}
	var ns corev1.Namespace
	if err := project.Get(ctx, ctrl.ObjectKey{Name: up.GetNamespace()}, &ns); err != nil {
		return err
	}
	hubKey := ctrl.ObjectKey{Namespace: MappedNamespace(string(ns.UID)), Name: up.GetName()}
	private, err := r.private(ctx, project, up)
	if err != nil {
		return err
	}
	if !up.GetDeletionTimestamp().IsZero() || !private {
		return r.withdraw(ctx, project, up, hubKey)
	}
	if !controllerutil.ContainsFinalizer(up, finalizer) {
		base := up.DeepCopy()
		controllerutil.AddFinalizer(up, finalizer)
		if err := project.Patch(ctx, up, ctrl.MergeFromWithOptions(base, ctrl.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	uid, err := r.ProjectUID(ctx, clusterName)
	if err != nil {
		return fmt.Errorf("resolve project %s: %w", clusterName, err)
	}
	if err := r.ensureNamespace(ctx, hubKey.Namespace, string(clusterName), ns.Name, uid); err != nil {
		return err
	}
	return r.copy(ctx, up, hubKey)
}

// private reports whether an object belongs to private DNS. Zones and record
// sets are shared with public DNS; every other kind is private only.
func (r *Replicator) private(ctx context.Context, project ctrl.Client, up *unstructured.Unstructured) (bool, error) {
	switch r.GVK.Kind {
	case "DNSZone":
		visibility, _, _ := unstructured.NestedString(up.Object, "spec", "visibility")
		return visibility == "Private", nil
	case "DNSRecordSet":
		zoneName, _, _ := unstructured.NestedString(up.Object, "spec", "dnsZoneRef", "name")
		zone := &unstructured.Unstructured{}
		zone.SetGroupVersionKind(gvk("DNSZone"))
		if err := project.Get(ctx, ctrl.ObjectKey{Namespace: up.GetNamespace(), Name: zoneName}, zone); apierrors.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		visibility, _, _ := unstructured.NestedString(zone.Object, "spec", "visibility")
		return visibility == "Private", nil
	}
	return true, nil
}

func (r *Replicator) ensureNamespace(ctx context.Context, name, cluster, upstreamNamespace string, projectUID types.UID) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err := controllerutil.CreateOrPatch(ctx, r.Hub, ns, func() error {
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		if ns.Annotations == nil {
			ns.Annotations = map[string]string{}
		}
		ns.Labels[ReplicatedLabel] = "true"
		ns.Annotations[UpstreamClusterAnnotation] = cluster
		ns.Annotations[UpstreamNamespaceAnnotation] = upstreamNamespace
		ns.Annotations[ProjectUIDAnnotation] = string(projectUID)
		return nil
	})
	return err
}

// copy writes the object's labels, annotations and spec into the hub. Status
// is never copied down: each region writes its own.
func (r *Replicator) copy(ctx context.Context, up *unstructured.Unstructured, key ctrl.ObjectKey) error {
	hub := &unstructured.Unstructured{}
	hub.SetGroupVersionKind(r.GVK)
	hub.SetNamespace(key.Namespace)
	hub.SetName(key.Name)
	_, err := controllerutil.CreateOrPatch(ctx, r.Hub, hub, func() error {
		if stored, ok := hub.GetAnnotations()[OriginGenerationAnnotation]; ok {
			if g, err := strconv.ParseInt(stored, 10, 64); err == nil && g > up.GetGeneration() {
				return nil
			}
		}
		labels := copyMap(up.GetLabels())
		labels[OriginLabel] = OriginProject
		labels[ReplicatedLabel] = "true"
		hub.SetLabels(labels)
		annotations := copyMap(up.GetAnnotations())
		delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
		annotations[OriginGenerationAnnotation] = strconv.FormatInt(up.GetGeneration(), 10)
		annotations[OriginUIDAnnotation] = string(up.GetUID())
		hub.SetAnnotations(annotations)
		spec, ok := up.Object["spec"]
		if ok && !equality.Semantic.DeepEqual(hub.Object["spec"], spec) {
			hub.Object["spec"] = runtimeDeepCopy(spec)
		}
		return nil
	})
	return err
}

// withdraw removes the hub copy, then releases the project object.
func (r *Replicator) withdraw(ctx context.Context, project ctrl.Client, up *unstructured.Unstructured, key ctrl.ObjectKey) error {
	if !controllerutil.ContainsFinalizer(up, finalizer) {
		return nil
	}
	hub := &unstructured.Unstructured{}
	hub.SetGroupVersionKind(r.GVK)
	err := r.Hub.Get(ctx, key, hub)
	if err == nil {
		if hub.GetLabels()[OriginLabel] != OriginProject {
			return fmt.Errorf("hub object %s is owned by origin %q, not the project", key, hub.GetLabels()[OriginLabel])
		}
		if err := r.Hub.Delete(ctx, hub); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	base := up.DeepCopy()
	controllerutil.RemoveFinalizer(up, finalizer)
	return project.Patch(ctx, up, ctrl.MergeFromWithOptions(base, ctrl.MergeFromWithOptimisticLock{}))
}

func copyMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func runtimeDeepCopy(v any) any {
	return (&unstructured.Unstructured{Object: map[string]any{"v": v}}).DeepCopy().Object["v"]
}
