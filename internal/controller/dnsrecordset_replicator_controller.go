package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
	mcsource "sigs.k8s.io/multicluster-runtime/pkg/source"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	downstreamclient "go.miloapis.com/dns-operator/internal/downstreamclient"
)

type DNSRecordSetReplicator struct {
	mgr              mcmanager.Manager
	DownstreamClient client.Client
}

const rsFinalizer = "dns.networking.miloapis.com/finalize-dnsrecordset"

// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets/finalizers,verbs=update
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch

func (r *DNSRecordSetReplicator) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
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
	if upstream.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(&upstream, rsFinalizer) {
		base := upstream.DeepCopy()
		controllerutil.AddFinalizer(&upstream, rsFinalizer)
		if err := upstreamCluster.GetClient().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
			log.FromContext(ctx).Error(err, "failed to add DNSRecordSet finalizer")
			return ctrl.Result{RequeueAfter: 300 * time.Millisecond}, nil
		}
		lg.Info("added upstream finalizer", "finalizer", rsFinalizer)
		// requeue to continue with updated object
		return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
	}

	// Deletion path: downstream-first via finalizer
	if !upstream.DeletionTimestamp.IsZero() {
		if err := r.handleDeletion(ctx, upstreamCluster.GetClient(), strategy, &upstream); err != nil {
			lg.Info("downstream DNSRecordSet still deleting; requeueing")
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		lg.Info("finalizer removed; allowing upstream DNSRecordSet to finalize")
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
		lg.Info("DNSZoneRef not set; marked Accepted=False and exiting early")
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
			lg.Info("referenced DNSZone not found; marked Accepted=False and exiting early", "dnsZone", upstream.Spec.DNSZoneRef.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	} else {
		zoneMsg = fmt.Sprintf("DNSZone %q exists", upstream.Spec.DNSZoneRef.Name)
	}

	// If the zone is being deleted, do not program downstream recordset
	if !zone.DeletionTimestamp.IsZero() {
		lg.Info("referenced DNSZone is deleting; skipping downstream programming", "dnsZone", zone.Name)
		return ctrl.Result{}, nil
	}

	// Ensure OwnerReference to upstream DNSZone (same ns)
	if !metav1.IsControlledBy(&upstream, &zone) {
		base := upstream.DeepCopy()
		if err := controllerutil.SetControllerReference(&zone, &upstream, upstreamCluster.GetScheme()); err != nil {
			return ctrl.Result{}, err
		}
		if err := upstreamCluster.GetClient().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		// ensure we continue with the updated object
		return ctrl.Result{}, nil
	}
	// Ensure the downstream recordset object mirrors the upstream spec
	if _, err = r.ensureDownstreamRecordSet(ctx, strategy, &upstream); err != nil {
		return ctrl.Result{}, err
	}

	// Mirror downstream Programmed condition into upstream, if present
	var downstreamProg *metav1.Condition
	if md, mdErr := strategy.ObjectMetaFromUpstreamObject(ctx, &upstream); mdErr == nil {
		var shadow dnsv1alpha1.DNSRecordSet
		if getErr := r.DownstreamClient.Get(ctx, types.NamespacedName{Namespace: md.Namespace, Name: md.Name}, &shadow); getErr == nil {
			if c := apimeta.FindStatusCondition(shadow.Status.Conditions, CondProgrammed); c != nil {
				downstreamProg = c.DeepCopy()
			}
		}
	}

	// Update upstream status: Accepted here; Programmed mirrored from downstream
	if err := r.updateStatus(ctx, upstreamCluster.GetClient(), &upstream, true, zoneMsg, downstreamProg); err != nil {
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

// handleDeletion deletes the downstream shadow object and removes the upstream finalizer
// once the shadow is confirmed gone. It returns nil only when the finalizer has been removed.
func (r *DNSRecordSetReplicator) handleDeletion(
	ctx context.Context,
	upstreamClient client.Client,
	strategy downstreamclient.ResourceStrategy,
	upstream *dnsv1alpha1.DNSRecordSet,
) error {
	// If no finalizer, nothing to enforce.
	if !controllerutil.ContainsFinalizer(upstream, rsFinalizer) {
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
	log.FromContext(ctx).Info("requested downstream delete for DNSRecordSet", "namespace", md.Namespace, "name", md.Name)

	// Confirm it's gone before removing the finalizer.
	var shadow dnsv1alpha1.DNSRecordSet
	getErr := r.DownstreamClient.Get(ctx, types.NamespacedName{Namespace: md.Namespace, Name: md.Name}, &shadow)
	if apierrors.IsNotFound(getErr) {
		base := upstream.DeepCopy()
		controllerutil.RemoveFinalizer(upstream, rsFinalizer)
		if err := upstreamClient.Patch(ctx, upstream, client.MergeFrom(base)); err != nil {
			return err
		}
		log.FromContext(ctx).Info("removed upstream finalizer", "finalizer", rsFinalizer)
		return nil
	}
	if getErr != nil {
		return getErr
	}

	// Still present—requeue caller by returning an error.
	log.FromContext(ctx).Info("downstream DNSRecordSet still exists; waiting", "namespace", md.Namespace, "name", md.Name)
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

	res, cErr := controllerutil.CreateOrPatch(ctx, r.DownstreamClient, &shadow, func() error {
		if shadow.Labels == nil {
			shadow.Labels = map[string]string{}
		}
		shadow.Labels = md.Labels
		if !equality.Semantic.DeepEqual(shadow.Spec, upstream.Spec) {
			shadow.Spec = upstream.Spec
		}
		return nil
	})
	if cErr != nil {
		return res, cErr
	}
	log.FromContext(ctx).Info("ensured downstream DNSRecordSet", "operation", res, "namespace", shadow.Namespace, "name", shadow.Name)
	return res, nil
}

// updateStatus sets Accepted locally and mirrors the Programmed condition from downstream when provided.
func (r *DNSRecordSetReplicator) updateStatus(ctx context.Context, c client.Client, upstream *dnsv1alpha1.DNSRecordSet, accepted bool, zoneMsg string, downstreamProg *metav1.Condition) error {
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

	// Programmed condition: mirror downstream when available; else mark pending
	if downstreamProg != nil {
		if setCond(&upstream.Status.Conditions, CondProgrammed, downstreamProg.Reason, downstreamProg.Message, downstreamProg.Status, upstream.Generation) {
			changed = true
		}
	} else {
		if setCond(&upstream.Status.Conditions, CondProgrammed, ReasonPending, "Awaiting downstream controller", metav1.ConditionFalse, upstream.Generation) {
			changed = true
		}
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

	b := mcbuilder.ControllerManagedBy(mgr)

	// Upstream watch (desired spec)
	b = b.For(&dnsv1alpha1.DNSRecordSet{})

	// Downstream watch (realized status → wake upstream owner)
	src := mcsource.TypedKind(
		&dnsv1alpha1.DNSRecordSet{},
		downstreamclient.TypedEnqueueRequestForUpstreamOwner[*dnsv1alpha1.DNSRecordSet](&dnsv1alpha1.DNSRecordSet{}),
	)
	clusterSrc, err := src.ForCluster("", downstreamCl)
	if err != nil {
		return fmt.Errorf("failed to build downstream watch for %s: %w", dnsv1alpha1.GroupVersion.WithKind("DNSRecordSet").String(), err)
	}
	b = b.WatchesRawSource(clusterSrc)

	return b.Named("dnsrecordset-replicator").Complete(r)
}
