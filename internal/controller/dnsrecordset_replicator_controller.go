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

// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch

// Reconcile flow (high-level):
// 1) Fetch upstream object
// 2) Handle deletion (best-effort downstream cleanup)
// 3) Gate on referenced DNSZone existence
// 4) Ensure downstream shadow via CreateOrPatch
// 5) Synthesize and update upstream Status conditions
func (r *DNSRecordSetReplicator) Reconcile(ctx context.Context, req GVKRequest) (ctrl.Result, error) {
	lg := log.FromContext(ctx).WithValues("cluster", req.ClusterName, "namespace", req.Namespace, "name", req.Name)
	ctx = log.IntoContext(ctx, lg)
	lg.Info("reconcile start")

	upstreamCluster, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if ok := upstreamCluster.GetCache().WaitForCacheSync(ctx); !ok {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	upstream, err := r.fetchUpstream(ctx, upstreamCluster, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	strategy := downstreamclient.NewMappedNamespaceResourceStrategy(req.ClusterName, upstreamCluster.GetClient(), r.DownstreamClient)

	// Deletion path: best-effort cleanup of the anchor and exit early
	if !upstream.DeletionTimestamp.IsZero() {
		if err := r.handleDeletion(ctx, strategy, &upstream); err != nil {
			lg.Error(err, "deleting anchor")
		}
		return ctrl.Result{}, nil
	}

	// Gate on referenced DNSZone
	accepted, zoneMsg, err := r.zoneAccepted(ctx, upstreamCluster.GetClient(), req.Namespace, upstream.Spec.DNSZoneRef.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Ensure the downstream shadow object mirrors the upstream spec
	op, err := r.ensureShadow(ctx, strategy, &upstream)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch op {
	case controllerutil.OperationResultCreated:
		lg.Info("created downstream DNSRecordSet")
	case controllerutil.OperationResultUpdated:
		lg.Info("updated downstream DNSRecordSet")
	}

	// Update upstream status conditions to reflect acceptance & programming state
	if err := r.updateStatus(ctx, upstreamCluster.GetClient(), &upstream, accepted, zoneMsg); err != nil {
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

func (r *DNSRecordSetReplicator) handleDeletion(ctx context.Context, strategy downstreamclient.ResourceStrategy, upstream *dnsv1alpha1.DNSRecordSet) error {
	return strategy.DeleteAnchorForObject(ctx, upstream)
}

// zoneAccepted resolves whether the referenced DNSZone exists and returns a human
// readable message for status.
func (r *DNSRecordSetReplicator) zoneAccepted(ctx context.Context, c client.Client, ns, zoneName string) (bool, string, error) {
	if zoneName == "" {
		return false, "DNSZoneRef not set", nil
	}
	var zone dnsv1alpha1.DNSZone
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: zoneName}, &zone)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("DNSZone %q not found", zoneName), nil
		}
		return false, "", err
	}
	return true, fmt.Sprintf("DNSZone %q exists", zoneName), nil
}

// ensureShadow idempotently mirrors upstream.Spec into a downstream shadow object.
func (r *DNSRecordSetReplicator) ensureShadow(ctx context.Context, strategy downstreamclient.ResourceStrategy, upstream *dnsv1alpha1.DNSRecordSet) (controllerutil.OperationResult, error) {
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
		return strategy.SetControllerReference(ctx, upstream, &shadow)
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
