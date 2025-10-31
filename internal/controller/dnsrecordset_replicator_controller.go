package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcsource "sigs.k8s.io/multicluster-runtime/pkg/source"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	downstreamclient "go.miloapis.com/dns-operator/internal/downstreamclient"
)

type DNSRecordSetReplicator struct {
	mgr              mcmanager.Manager
	DownstreamClient client.Client
}

const RSFinalizer = "dns.networking.miloapis.com/finalize-dnsrecordset"

// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets/finalizers,verbs=update
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch

func (r *DNSRecordSetReplicator) Reconcile(ctx context.Context, req GVKRequest) (ctrl.Result, error) {
	lg := log.FromContext(ctx).WithValues("cluster", req.ClusterName, "namespace", req.Namespace, "name", req.Name)
	ctx = log.IntoContext(ctx, lg)
	lg.Info("reconcile start")

	upstreamCluster, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	upstream, err := r.fetchUpstream(ctx, upstreamCluster, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	strategy := downstreamclient.NewMappedNamespaceResourceStrategy(req.ClusterName, upstreamCluster.GetClient(), r.DownstreamClient)

	// Ensure upstream finalizer (non-deletion path; replaces webhook defaulter)
	if upstream.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(&upstream, RSFinalizer) {
		base := upstream.DeepCopy()
		controllerutil.AddFinalizer(&upstream, RSFinalizer)
		if err := upstreamCluster.GetClient().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
			log.FromContext(ctx).Error(err, "failed to add DNSRecordSet finalizer")
			return ctrl.Result{RequeueAfter: 300 * time.Millisecond}, nil
		}
		// requeue to continue with updated object
		return ctrl.Result{}, nil
	}

	// Deletion path: downstream-first via finalizer
	if !upstream.DeletionTimestamp.IsZero() {
		if err := r.handleDeletion(ctx, upstreamCluster.GetClient(), strategy, &upstream); err != nil {
			lg.V(1).Info("downstream still deleting; requeueing")
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		// finalizer removed; allow upstream object to finalize
		return ctrl.Result{}, nil
	}

	// Gate on referenced DNSZone early and update status when missing
	var zoneMsg string
	if upstream.Spec.DNSZoneRef.Name == "" {
		zoneMsg = "DNSZoneRef not set"
		if setCond(&upstream.Status.Conditions, CondAccepted, ReasonPending, zoneMsg, metav1.ConditionFalse, upstream.Generation) {
			_ = upstreamCluster.GetClient().Status().Update(ctx, &upstream)
		}
		return ctrl.Result{}, nil
	}
	var zone dnsv1alpha1.DNSZone
	if err := upstreamCluster.GetClient().Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: upstream.Spec.DNSZoneRef.Name}, &zone); err != nil {
		if apierrors.IsNotFound(err) {
			zoneMsg = fmt.Sprintf("DNSZone %q not found", upstream.Spec.DNSZoneRef.Name)
			if setCond(&upstream.Status.Conditions, CondAccepted, ReasonPending, zoneMsg, metav1.ConditionFalse, upstream.Generation) {
				if err := upstreamCluster.GetClient().Status().Update(ctx, &upstream); err != nil {
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	} else {
		zoneMsg = fmt.Sprintf("DNSZone %q exists", upstream.Spec.DNSZoneRef.Name)
	}

	// Ensure OwnerReference to upstream DNSZone (same ns)
	if updated, err := ensureOwnerRefToZone(ctx, upstreamCluster.GetClient(), &upstream, &zone); err != nil {
		return ctrl.Result{}, err
	} else if updated {
		// only requeue once after successful patch
		return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
	}

	// Ensure the downstream recordset object mirrors the upstream spec
	if _, err = r.ensureDownstreamRecordSet(ctx, strategy, &upstream); err != nil {
		return ctrl.Result{}, err
	}

	// Update upstream status conditions to reflect acceptance & programming state
	if err := r.updateStatus(ctx, upstreamCluster.GetClient(), &upstream, true, zoneMsg); err != nil {
		if !apierrors.IsNotFound(err) { // tolerate races
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// ---- Helpers ---------------------------------------------------------------

func (r *DNSRecordSetReplicator) fetchUpstream(ctx context.Context, cl cluster.Cluster, nn types.NamespacedName) (dnsv1alpha1.DNSRecordSet, error) {
	var upstream dnsv1alpha1.DNSRecordSet
	if err := cl.GetClient().Get(ctx, nn, &upstream); err != nil {
		return dnsv1alpha1.DNSRecordSet{}, err
	}
	return upstream, nil
}

// ensureOwnerRefToZone ensures a single OwnerReference to the current zone.
// It replaces any existing DNSZone owner refs (stale UID / changed zone).
func ensureOwnerRefToZone(ctx context.Context, c client.Client, rs *dnsv1alpha1.DNSRecordSet, zone *dnsv1alpha1.DNSZone) (bool, error) {
	desired := metav1.OwnerReference{
		APIVersion: dnsv1alpha1.GroupVersion.String(),
		Kind:       "DNSZone",
		Name:       zone.Name,
		UID:        zone.UID,
	}

	// Check if already identical
	for _, r := range rs.OwnerReferences {
		if r.APIVersion == desired.APIVersion &&
			r.Kind == desired.Kind &&
			r.Name == desired.Name &&
			r.UID == desired.UID {
			return false, nil // nothing to do
		}
	}

	// Remove any old DNSZone refs
	newRefs := make([]metav1.OwnerReference, 0, len(rs.OwnerReferences))
	for _, r := range rs.OwnerReferences {
		if r.APIVersion == dnsv1alpha1.GroupVersion.String() && r.Kind == "DNSZone" {
			continue
		}
		newRefs = append(newRefs, r)
	}

	base := rs.DeepCopy()
	rs.OwnerReferences = append(newRefs, desired)
	if err := c.Patch(ctx, rs, client.MergeFrom(base)); err != nil {
		return false, err
	}

	return true, nil
}

// handleDeletion deletes the downstream shadow object and removes the upstream finalizer
// once the shadow is confirmed gone. It returns nil only when the finalizer has been removed.
func (r *DNSRecordSetReplicator) handleDeletion(
	ctx context.Context,
	upstreamClient client.Client,
	strategy downstreamclient.ResourceStrategy,
	upstream *dnsv1alpha1.DNSRecordSet,
) error {
	// If no finalizer, nothing to enforce.
	if !controllerutil.ContainsFinalizer(upstream, RSFinalizer) {
		return nil
	}

	// Compute downstream name/namespace for this upstream object.
	md, err := strategy.ObjectMetaFromUpstreamObject(ctx, upstream)
	if err != nil {
		return err
	}

	// Issue delete for the downstream shadow (idempotent).
	err = r.DownstreamClient.Delete(ctx, &dnsv1alpha1.DNSRecordSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "dns.networking.miloapis.com/v1alpha1",
			Kind:       "DNSRecordSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: md.Namespace,
			Name:      md.Name,
		},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	// Confirm it's gone before removing the finalizer.
	var shadow dnsv1alpha1.DNSRecordSet
	if err := r.DownstreamClient.Get(ctx, types.NamespacedName{Namespace: md.Namespace, Name: md.Name}, &shadow); err != nil {
		if apierrors.IsNotFound(err) {
			// Safe to remove finalizer now.
			controllerutil.RemoveFinalizer(upstream, RSFinalizer)
			return upstreamClient.Update(ctx, upstream)
		}
		return err
	}

	// Still present—requeue caller by returning an error.
	return fmt.Errorf("downstream DNSRecordSet %s/%s still deleting", md.Namespace, md.Name)
}

// ensureDownstreamRecordSet idempotently mirrors upstream.Spec into a downstream shadow object.
func (r *DNSRecordSetReplicator) ensureDownstreamRecordSet(ctx context.Context, strategy downstreamclient.ResourceStrategy, upstream *dnsv1alpha1.DNSRecordSet) (controllerutil.OperationResult, error) {
	md, err := strategy.ObjectMetaFromUpstreamObject(ctx, upstream)
	if err != nil {
		return controllerutil.OperationResultNone, err
	}

	shadow := dnsv1alpha1.DNSRecordSet{}
	shadow.SetGroupVersionKind(schema.GroupVersionKind{Group: "dns.networking.miloapis.com", Version: "v1alpha1", Kind: "DNSRecordSet"})
	shadow.SetNamespace(md.Namespace)
	shadow.SetName(md.Name)

	return controllerutil.CreateOrPatch(ctx, r.DownstreamClient, &shadow, func() error {
		if shadow.Labels == nil {
			shadow.Labels = map[string]string{}
		}
		shadow.Labels = md.Labels
		if !equality.Semantic.DeepEqual(shadow.Spec, upstream.Spec) {
			shadow.Spec = upstream.Spec
		}
		return nil
	})
}

// updateStatus synthesizes the Accepted and Programmed conditions and performs a Status().Update when changes occur.
func (r *DNSRecordSetReplicator) updateStatus(ctx context.Context, c client.Client, upstream *dnsv1alpha1.DNSRecordSet, accepted bool, zoneMsg string) error {
	changed := false

	// Accepted condition
	if accepted {
		if setCond(&upstream.Status.Conditions, CondAccepted, ReasonAccepted, zoneMsg, metav1.ConditionTrue, upstream.Generation) {
			changed = true
		}
	} else {
		if setCond(&upstream.Status.Conditions, CondAccepted, ReasonPending, zoneMsg, metav1.ConditionFalse, upstream.Generation) {
			changed = true
		}
	}

	// Programmed condition: true when we've run ensureShadow (i.e., this function is called post-CreateOrPatch) AND the zone is accepted
	progMsg := "Awaiting DNSZone and downstream creation"
	progStatus := metav1.ConditionFalse
	progReason := ReasonPending
	if accepted {
		progMsg = "Downstream shadow exists"
		progStatus = metav1.ConditionTrue
		progReason = ReasonProgrammed
	}
	if setCond(&upstream.Status.Conditions, CondProgrammed, progReason, progMsg, progStatus, upstream.Generation) {
		changed = true
	}

	if !changed {
		return nil
	}
	if err := c.Status().Update(ctx, upstream); err != nil {
		return err
	}

	log.FromContext(ctx).Info("upstream recordset status updated",
		"accepted", getCondStatus(upstream.Status.Conditions, CondAccepted),
		"programmed", getCondStatus(upstream.Status.Conditions, CondProgrammed),
	)
	return nil
}

// ---- Watches / mapping helpers -------------------------
func (r *DNSRecordSetReplicator) SetupWithManager(mgr mcmanager.Manager, downstreamCl cluster.Cluster) error {
	r.mgr = mgr

	b := mcbuilder.TypedControllerManagedBy[GVKRequest](mgr)

	// Upstream watch (desired spec)
	rsGVK := schema.GroupVersionKind{Group: "dns.networking.miloapis.com", Version: "v1alpha1", Kind: "DNSRecordSet"}
	upstreamRS := newUnstructuredForGVK(rsGVK)
	b = b.Watches(upstreamRS, typedEnqueueRequestForGVK(rsGVK))

	// Downstream watch (realized status → wake upstream owner)
	downstreamRS := newUnstructuredForGVK(rsGVK)
	src := mcsource.TypedKind(
		downstreamRS,
		typedEnqueueDownstreamGVKRequest(rsGVK),
	)
	clusterSrc, err := src.ForCluster("", downstreamCl)
	if err != nil {
		return fmt.Errorf("failed to build downstream watch for %s: %w", rsGVK.String(), err)
	}
	b = b.WatchesRawSource(clusterSrc)

	return b.Named("dnsrecordset-replicator").Complete(r)
}
