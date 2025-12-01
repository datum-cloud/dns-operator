package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			return ctrl.Result{}, err
		}
		lg.Info("added upstream finalizer", "finalizer", rsFinalizer)
		return ctrl.Result{}, nil
	}

	// Deletion path: downstream-first via finalizer
	if !upstream.DeletionTimestamp.IsZero() {
		done, err := r.handleDeletion(ctx, upstreamCluster.GetClient(), strategy, &upstream)
		if err != nil {
			lg.Error(err, "deletion handling error; requeueing")
			return ctrl.Result{}, err
		}
		if !done {
			lg.Info("downstream DNSRecordSet still deleting; waiting for downstream update")
			return ctrl.Result{}, nil
		}
		lg.Info("finalizer removed; allowing upstream DNSRecordSet to finalize")
		// finalizer removed; allow upstream object to finalize
		return ctrl.Result{}, nil
	}

	// Gate on referenced DNSZone early and update status when missing
	var zoneMsg string
	var zone dnsv1alpha1.DNSZone
	if err := upstreamCluster.GetClient().Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: upstream.Spec.DNSZoneRef.Name}, &zone); err != nil {
		if apierrors.IsNotFound(err) {
			zoneMsg = fmt.Sprintf("DNSZone %q not found", upstream.Spec.DNSZoneRef.Name)
			if apimeta.SetStatusCondition(&upstream.Status.Conditions, metav1.Condition{
				Type:               CondAccepted,
				Status:             metav1.ConditionFalse,
				Reason:             ReasonPending,
				Message:            zoneMsg,
				ObservedGeneration: upstream.Generation,
				LastTransitionTime: metav1.NewTime(time.Now()),
			}) {
				base := upstream.DeepCopy()
				if err := upstreamCluster.GetClient().Status().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
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

	// Mirror downstream status (conditions + records) when the shadow exists
	md, mdErr := strategy.ObjectMetaFromUpstreamObject(ctx, &upstream)
	if mdErr != nil {
		return ctrl.Result{}, mdErr
	}
	var shadow dnsv1alpha1.DNSRecordSet
	if err := r.DownstreamClient.Get(ctx, types.NamespacedName{Namespace: md.Namespace, Name: md.Name}, &shadow); err != nil {
		return ctrl.Result{}, err
	}

	// Update upstream status by mirroring downstream when present
	if err := r.updateStatus(ctx, upstreamCluster.GetClient(), &upstream, shadow.Status.DeepCopy()); err != nil {
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
// once the shadow is confirmed gone. It returns done=true only when the finalizer has been removed
// (or when no finalizer is present). When the downstream still exists, it returns done=false, nil.
func (r *DNSRecordSetReplicator) handleDeletion(
	ctx context.Context,
	upstreamClient client.Client,
	strategy downstreamclient.ResourceStrategy,
	upstream *dnsv1alpha1.DNSRecordSet,
) (bool, error) {
	// If no finalizer, nothing to enforce.
	if !controllerutil.ContainsFinalizer(upstream, rsFinalizer) {
		return true, nil
	}

	// Compute downstream name/namespace for this upstream object.
	md, err := strategy.ObjectMetaFromUpstreamObject(ctx, upstream)
	if err != nil {
		return false, err
	}

	// Check if downstream shadow exists first.
	var shadow dnsv1alpha1.DNSRecordSet
	getErr := r.DownstreamClient.Get(ctx, types.NamespacedName{Namespace: md.Namespace, Name: md.Name}, &shadow)
	if apierrors.IsNotFound(getErr) {
		base := upstream.DeepCopy()
		controllerutil.RemoveFinalizer(upstream, rsFinalizer)
		if err := upstreamClient.Patch(ctx, upstream, client.MergeFrom(base)); err != nil {
			return false, err
		}
		log.FromContext(ctx).Info("removed upstream finalizer", "finalizer", rsFinalizer)
		return true, nil
	}
	if getErr != nil {
		return false, getErr
	}

	// If the shadow is not already deleting, issue a delete now.
	if shadow.DeletionTimestamp.IsZero() {
		err = r.DownstreamClient.Delete(ctx, &shadow)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		log.FromContext(ctx).Info("requested downstream delete for DNSRecordSet", "namespace", md.Namespace, "name", md.Name)
	} else {
		log.FromContext(ctx).Info("downstream DNSRecordSet already deleting; waiting", "namespace", md.Namespace, "name", md.Name)
	}

	// Still present—signal not done to trigger requeue by caller.
	return false, nil
}

// ensureDownstreamRecordSet idempotently mirrors upstream.Spec into a downstream shadow object.
func (r *DNSRecordSetReplicator) ensureDownstreamRecordSet(ctx context.Context, strategy downstreamclient.ResourceStrategy, upstream *dnsv1alpha1.DNSRecordSet) (controllerutil.OperationResult, error) {
	md, err := strategy.ObjectMetaFromUpstreamObject(ctx, upstream)
	if err != nil {
		return controllerutil.OperationResultNone, err
	}

	shadow := dnsv1alpha1.DNSRecordSet{}
	shadow.SetNamespace(md.Namespace)
	shadow.SetName(md.Name)

	res, cErr := controllerutil.CreateOrPatch(ctx, r.DownstreamClient, &shadow, func() error {
		shadow.Labels = md.Labels
		if !equality.Semantic.DeepEqual(shadow.Spec, upstream.Spec) {
			shadow.Spec = upstream.Spec
		}
		return strategy.SetControllerReference(ctx, upstream, &shadow)
	})
	if cErr != nil {
		return res, cErr
	}
	log.FromContext(ctx).Info("ensured downstream DNSRecordSet", "operation", res, "namespace", shadow.Namespace, "name", shadow.Name)
	return res, nil
}

// updateStatus sets Accepted locally and mirrors the Programmed condition from downstream when provided.
func (r *DNSRecordSetReplicator) updateStatus(
	ctx context.Context,
	c client.Client,
	upstream *dnsv1alpha1.DNSRecordSet,
	downstreamStatus *dnsv1alpha1.DNSRecordSetStatus,
) error {
	if downstreamStatus == nil {
		return nil
	}

	if equality.Semantic.DeepEqual(upstream.Status, *downstreamStatus) {
		return nil
	}

	base := upstream.DeepCopy()
	upstream.Status = *downstreamStatus
	return c.Status().Patch(ctx, upstream, client.MergeFrom(base))
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
