// SPDX-License-Identifier: AGPL-3.0-only

package privatereplication

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/projectstatus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
	mcsource "sigs.k8s.io/multicluster-runtime/pkg/source"
)

// ResourceBindingGVK is the hub's record of where an object is placed. Its
// status carries each member cluster's copy of the object's status.
var ResourceBindingGVK = schema.GroupVersionKind{Group: "work.karmada.io", Version: "v1alpha2", Kind: "ResourceBinding"}

const mirrorFinalizer = "internal-dns.miloapis.com/mirror"

// StatusMirror merges every region's status entries for an object, as the hub
// collected them from each region, into the project object, and sets its
// Published condition from them. Each region writes only its own entry, so
// the merge never decides between two writers.
type StatusMirror struct {
	Hub ctrl.Client
	mgr mcmanager.Manager
}

func (m *StatusMirror) SetupWithManager(mgr mcmanager.Manager, hub cluster.Cluster) error {
	m.mgr = mgr
	rb := &unstructured.Unstructured{}
	rb.SetGroupVersionKind(ResourceBindingGVK)
	src, _, err := mcsource.TypedKind(ctrl.Object(rb), mchandler.TypedEnqueueRequestForObject[ctrl.Object]()).ForCluster("", hub)
	if err != nil {
		return err
	}
	return mcbuilder.ControllerManagedBy(mgr).Named("private-status-mirror").WatchesRawSource(src).Complete(m)
}

func (m *StatusMirror) Reconcile(ctx context.Context, req mcreconcile.Request) (reconcile.Result, error) {
	return reconcile.Result{}, m.mirror(ctx, req.NamespacedName, func(namespace string) (ctrl.Client, string, error) {
		return upstreamOf(ctx, m.Hub, m.mgr, namespace)
	})
}

// resolver finds the project client and namespace for a mapped hub namespace.
type resolver func(namespace string) (ctrl.Client, string, error)

func (m *StatusMirror) mirror(ctx context.Context, key types.NamespacedName, resolve resolver) error {
	rb := &unstructured.Unstructured{}
	rb.SetGroupVersionKind(ResourceBindingGVK)
	if err := m.Hub.Get(ctx, key, rb); err != nil {
		return ctrl.IgnoreNotFound(err)
	}
	kind, _, _ := unstructured.NestedString(rb.Object, "spec", "resource", "kind")
	apiVersion, _, _ := unstructured.NestedString(rb.Object, "spec", "resource", "apiVersion")
	name, _, _ := unstructured.NestedString(rb.Object, "spec", "resource", "name")
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil || !known(gv.WithKind(kind)) {
		return nil
	}
	entries, err := regionEntries(rb)
	if err != nil {
		return err
	}
	placed, _, _ := unstructured.NestedSlice(rb.Object, "spec", "clusters")
	if len(entries) == 0 {
		return nil
	}
	project, upstreamNamespace, err := resolve(key.Namespace)
	if err != nil || project == nil {
		return err
	}
	up := &unstructured.Unstructured{}
	up.SetGroupVersionKind(gv.WithKind(kind))
	if err := project.Get(ctx, ctrl.ObjectKey{Namespace: upstreamNamespace, Name: name}, up); err != nil {
		return ctrl.IgnoreNotFound(err)
	}
	base := up.DeepCopy()
	if !applyRegions(up, entries, len(placed), time.Now().UTC()) {
		return nil
	}
	return project.Status().Patch(ctx, up, ctrl.MergeFromWithOptions(base, ctrl.MergeFromWithOptimisticLock{}))
}

// regionEntries collects status.regions from every member's copy. A region
// whose entry appears in two members keeps the newer one.
func regionEntries(rb *unstructured.Unstructured) ([]dnsv1alpha1.DNSRegionPublicationStatus, error) {
	members, _, _ := unstructured.NestedSlice(rb.Object, "status", "aggregatedStatus")
	byRegion := map[string]dnsv1alpha1.DNSRegionPublicationStatus{}
	for _, member := range members {
		raw, ok, _ := unstructured.NestedFieldNoCopy(member.(map[string]any), "status", "regions")
		if !ok {
			continue
		}
		b, err := json.Marshal(raw)
		if err != nil {
			return nil, err
		}
		var regions []dnsv1alpha1.DNSRegionPublicationStatus
		if err := json.Unmarshal(b, &regions); err != nil {
			return nil, fmt.Errorf("decode member status: %w", err)
		}
		for _, e := range regions {
			if prev, ok := byRegion[e.Region]; ok && newer(prev, e) {
				continue
			}
			byRegion[e.Region] = e
		}
	}
	out := make([]dnsv1alpha1.DNSRegionPublicationStatus, 0, len(byRegion))
	for _, e := range byRegion {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Region < out[j].Region })
	return out, nil
}

func newer(a, b dnsv1alpha1.DNSRegionPublicationStatus) bool {
	if a.ObservedGeneration != b.ObservedGeneration {
		return a.ObservedGeneration > b.ObservedGeneration
	}
	if a.WriterEpoch != b.WriterEpoch {
		return a.WriterEpoch > b.WriterEpoch
	}
	return a.Revision >= b.Revision
}

// applyRegions merges the members' entries into the object's status.regions,
// replacing each reporting region's entry and keeping the others, which a
// control plane reading the project API may have written itself. Published
// waits for every region with an entry and, while fewer members have
// reported than the hub placed the object in, for the rest.
func applyRegions(up *unstructured.Unstructured, entries []dnsv1alpha1.DNSRegionPublicationStatus, placed int, now time.Time) bool {
	var status struct {
		Regions    []dnsv1alpha1.DNSRegionPublicationStatus `json:"regions,omitempty"`
		Conditions []metav1.Condition                       `json:"conditions,omitempty"`
	}
	if raw, ok := up.Object["status"]; ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &status)
	}
	before, _ := json.Marshal(status)
	for _, e := range entries {
		projectstatus.SetRegion(&status.Regions, e)
	}
	expected := make([]string, 0, len(status.Regions))
	for _, e := range status.Regions {
		expected = append(expected, e.Region)
	}
	pending := projectstatus.PendingRegions(status.Regions, expected, up.GetGeneration())
	if missing := placed - len(entries); missing > 0 {
		pending = append(pending, fmt.Sprintf("%d more", missing))
	}
	projectstatus.PublishedInRegions(&status.Conditions, pending, up.GetGeneration(), now)
	after, _ := json.Marshal(status)
	if string(before) == string(after) {
		return false
	}
	var merged map[string]any
	_ = json.Unmarshal(after, &merged)
	current, _ := up.Object["status"].(map[string]any)
	if current == nil {
		current = map[string]any{}
	}
	for k, v := range merged {
		current[k] = v
	}
	up.Object["status"] = current
	return true
}

// ObjectMirror copies objects a region wrote itself from the hub into their
// project, so the project API records them. The copy keeps the region as its
// origin, so the replicator never sends it back down.
type ObjectMirror struct {
	GVK schema.GroupVersionKind
	Hub ctrl.Client
	mgr mcmanager.Manager
}

func (m *ObjectMirror) SetupWithManager(mgr mcmanager.Manager, hub cluster.Cluster) error {
	m.mgr = mgr
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(m.GVK)
	src, _, err := mcsource.TypedKind(ctrl.Object(obj), mchandler.TypedEnqueueRequestForObject[ctrl.Object]()).ForCluster("", hub)
	if err != nil {
		return err
	}
	return mcbuilder.ControllerManagedBy(mgr).Named("private-object-mirror-" + strings.ToLower(m.GVK.Kind)).WatchesRawSource(src).Complete(m)
}

func (m *ObjectMirror) Reconcile(ctx context.Context, req mcreconcile.Request) (reconcile.Result, error) {
	return reconcile.Result{}, m.mirror(ctx, req.NamespacedName, func(namespace string) (ctrl.Client, string, error) {
		return upstreamOf(ctx, m.Hub, m.mgr, namespace)
	})
}

func (m *ObjectMirror) mirror(ctx context.Context, hubKey types.NamespacedName, resolve resolver) error {
	hub := &unstructured.Unstructured{}
	hub.SetGroupVersionKind(m.GVK)
	if err := m.Hub.Get(ctx, hubKey, hub); err != nil {
		return ctrl.IgnoreNotFound(err)
	}
	origin := hub.GetLabels()[OriginLabel]
	if origin == "" || origin == OriginProject {
		// Not a region's write, or no longer one: release it if this mirror
		// held it, so it can finish deleting.
		if controllerutil.ContainsFinalizer(hub, mirrorFinalizer) {
			base := hub.DeepCopy()
			controllerutil.RemoveFinalizer(hub, mirrorFinalizer)
			return m.Hub.Patch(ctx, hub, ctrl.MergeFromWithOptions(base, ctrl.MergeFromWithOptimisticLock{}))
		}
		return nil
	}
	project, upstreamNamespace, err := resolve(hubKey.Namespace)
	if err != nil || project == nil {
		return err
	}
	key := ctrl.ObjectKey{Namespace: upstreamNamespace, Name: hub.GetName()}
	if !hub.GetDeletionTimestamp().IsZero() {
		return m.withdraw(ctx, project, hub, key, origin)
	}
	if !controllerutil.ContainsFinalizer(hub, mirrorFinalizer) {
		base := hub.DeepCopy()
		controllerutil.AddFinalizer(hub, mirrorFinalizer)
		if err := m.Hub.Patch(ctx, hub, ctrl.MergeFromWithOptions(base, ctrl.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	up := &unstructured.Unstructured{}
	up.SetGroupVersionKind(m.GVK)
	up.SetNamespace(key.Namespace)
	up.SetName(key.Name)
	if err := project.Get(ctx, key, up); err == nil && up.GetLabels()[OriginLabel] != origin {
		return fmt.Errorf("project object %s has origin %q, not %q; refusing to overwrite it", key, up.GetLabels()[OriginLabel], origin)
	} else if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	_, err = controllerutil.CreateOrPatch(ctx, project, up, func() error {
		labels := withoutReserved(hub.GetLabels())
		labels[OriginLabel] = origin
		up.SetLabels(labels)
		annotations := withoutReserved(hub.GetAnnotations())
		annotations[OriginGenerationAnnotation] = strconv.FormatInt(hub.GetGeneration(), 10)
		up.SetAnnotations(annotations)
		up.Object["spec"] = runtimeDeepCopy(hub.Object["spec"])
		return nil
	})
	return err
}

func (m *ObjectMirror) withdraw(ctx context.Context, project ctrl.Client, hub *unstructured.Unstructured, key ctrl.ObjectKey, origin string) error {
	if !controllerutil.ContainsFinalizer(hub, mirrorFinalizer) {
		return nil
	}
	up := &unstructured.Unstructured{}
	up.SetGroupVersionKind(m.GVK)
	if err := project.Get(ctx, key, up); err == nil {
		if up.GetLabels()[OriginLabel] == origin {
			if err := project.Delete(ctx, up); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	base := hub.DeepCopy()
	controllerutil.RemoveFinalizer(hub, mirrorFinalizer)
	return m.Hub.Patch(ctx, hub, ctrl.MergeFromWithOptions(base, ctrl.MergeFromWithOptimisticLock{}))
}

// upstreamOf finds the project client and namespace a mapped hub namespace
// stands for. A namespace the replicator did not map has none.
func upstreamOf(ctx context.Context, hub ctrl.Client, mgr mcmanager.Manager, namespace string) (ctrl.Client, string, error) {
	var ns corev1.Namespace
	if err := hub.Get(ctx, ctrl.ObjectKey{Name: namespace}, &ns); err != nil {
		return nil, "", ctrl.IgnoreNotFound(err)
	}
	clusterName, upstreamNamespace := ns.Annotations[UpstreamClusterAnnotation], ns.Annotations[UpstreamNamespaceAnnotation]
	if clusterName == "" || upstreamNamespace == "" {
		return nil, "", nil
	}
	cl, err := mgr.GetCluster(ctx, multicluster.ClusterName(clusterName))
	if err != nil {
		return nil, "", err
	}
	return cl.GetClient(), upstreamNamespace, nil
}

func known(g schema.GroupVersionKind) bool {
	for _, k := range Kinds {
		if k == g {
			return true
		}
	}
	return false
}
