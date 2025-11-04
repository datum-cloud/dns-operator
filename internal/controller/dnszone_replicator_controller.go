package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	crreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
	mcsource "sigs.k8s.io/multicluster-runtime/pkg/source"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	downstreamclient "go.miloapis.com/dns-operator/internal/downstreamclient"
)

type DNSZoneReplicator struct {
	mgr              mcmanager.Manager
	DownstreamClient client.Client
}

const DNSZoneFinalizer = "dns.networking.miloapis.com/finalize-dnszone"

// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones/finalizers,verbs=update;patch;delete
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *DNSZoneReplicator) Reconcile(ctx context.Context, req GVKRequest) (ctrl.Result, error) {
	lg := log.FromContext(ctx).WithValues("cluster", req.ClusterName, "namespace", req.Namespace, "name", req.Name)
	ctx = log.IntoContext(ctx, lg)
	lg.Info("reconcile start")

	upstreamCl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 1) Fetch upstream
	upstream, err := r.fetchUpstream(ctx, upstreamCl, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// --- Ensure finalizer on creation/update (non-deletion path) ---
	if upstream.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&upstream, DNSZoneFinalizer) {
			base := upstream.DeepCopy()
			upstream.Finalizers = append(upstream.Finalizers, DNSZoneFinalizer)
			if err := upstreamCl.GetClient().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
				lg.Error(err, "failed to add upstream finalizer")
				return ctrl.Result{RequeueAfter: 300 * time.Millisecond}, nil
			}
			// Re-run with updated object
			return ctrl.Result{}, nil
		}
	} else {
		// Deletion guard: ensure downstream is gone before removing finalizer
		if controllerutil.ContainsFinalizer(&upstream, DNSZoneFinalizer) {
			strategy := downstreamclient.NewMappedNamespaceResourceStrategy(req.ClusterName, upstreamCl.GetClient(), r.DownstreamClient)

			// Request deletion of downstream anchor/shadow
			if err := r.handleDeletion(ctx, strategy, &upstream); err != nil && !apierrors.IsNotFound(err) {
				lg.Error(err, "downstream delete failed; will retry")
				return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
			}

			// Verify downstream object is actually gone before dropping finalizer
			md, mdErr := strategy.ObjectMetaFromUpstreamObject(ctx, &upstream)
			if mdErr != nil {
				lg.Error(mdErr, "failed to compute downstream metadata; will retry")
				return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
			}
			var shadow dnsv1alpha1.DNSZone
			shadow.SetNamespace(md.Namespace)
			shadow.SetName(md.Name)
			getErr := r.DownstreamClient.Get(ctx, client.ObjectKey{Namespace: md.Namespace, Name: md.Name}, &shadow)
			if getErr == nil {
				// Still exists; rely on downstream watch to requeue when it changes
				return ctrl.Result{}, nil
			}
			if !apierrors.IsNotFound(getErr) {
				// Transient error; retry
				lg.Error(getErr, "failed to check downstream deletion; will retry")
				return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
			}

			// Downstream is confirmed gone -> remove upstream finalizer (conflict-safe)
			base := upstream.DeepCopy()
			controllerutil.RemoveFinalizer(&upstream, DNSZoneFinalizer)
			if err := upstreamCl.GetClient().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
				lg.Error(err, "failed to remove upstream finalizer")
				return ctrl.Result{RequeueAfter: 300 * time.Millisecond}, nil
			}
		}
		return ctrl.Result{}, nil
	}

	// If DNSZoneClassName is not set, set Accepted=False with reason
	if upstream.Spec.DNSZoneClassName == "" {
		if setCond(&upstream.Status.Conditions, CondAccepted, ReasonPending, "DNSZoneClassName not set", metav1.ConditionFalse, upstream.Generation) {
			err = upstreamCl.GetClient().Status().Update(ctx, &upstream)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	strategy := downstreamclient.NewMappedNamespaceResourceStrategy(req.ClusterName, upstreamCl.GetClient(), r.DownstreamClient)

	// 2) Deletion guard
	if !upstream.DeletionTimestamp.IsZero() {
		if err := r.handleDeletion(ctx, strategy, &upstream); err != nil {
			lg.Error(err, "deleting anchor")
		}
		return ctrl.Result{}, nil
	}

	// 3) Resolve DNSZoneClass early; if specified but not found, set Accepted=False with message and stop
	var zoneClass dnsv1alpha1.DNSZoneClass
	if err := upstreamCl.GetClient().Get(ctx, client.ObjectKey{Name: upstream.Spec.DNSZoneClassName}, &zoneClass); err != nil {
		if apierrors.IsNotFound(err) {
			// Update status: Accepted=False with reason
			if setCond(&upstream.Status.Conditions, CondAccepted, ReasonPending, fmt.Sprintf("DNSZoneClass %q not found", upstream.Spec.DNSZoneClassName), metav1.ConditionFalse, upstream.Generation) {
				err = upstreamCl.GetClient().Status().Update(ctx, &upstream)
				if err != nil {
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 4) Ensure downstream shadow (only after class presence check)
	_, err = r.ensureDownstreamZone(ctx, strategy, &upstream)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 5) First, refresh upstream status from downstream (populate nameservers)
	if err := r.updateStatus(ctx, upstreamCl.GetClient(), strategy, &upstream); err != nil {
		if !apierrors.IsNotFound(err) { // tolerate races
			return ctrl.Result{}, err
		}
	}

	// 6) Ensure default records exist only after nameservers are available from downstream
	if len(upstream.Status.Nameservers) > 0 {
		if err := r.ensureSOARecordSet(ctx, upstreamCl.GetClient(), &upstream); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.ensureNSRecordSet(ctx, upstreamCl.GetClient(), &upstream); err != nil {
			return ctrl.Result{}, err
		}
		// Recompute status to set Programmed based on record presence
		if err := r.updateStatus(ctx, upstreamCl.GetClient(), strategy, &upstream); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{}, nil
}

// ---- Helpers ---------------------------------------------------------------

func (r *DNSZoneReplicator) fetchUpstream(ctx context.Context, cl cluster.Cluster, nn types.NamespacedName) (dnsv1alpha1.DNSZone, error) {
	var upstream dnsv1alpha1.DNSZone
	if err := cl.GetClient().Get(ctx, nn, &upstream); err != nil {
		return dnsv1alpha1.DNSZone{}, err
	}
	return upstream, nil
}

func (r *DNSZoneReplicator) handleDeletion(ctx context.Context, strategy downstreamclient.ResourceStrategy, upstream *dnsv1alpha1.DNSZone) error {
	return strategy.DeleteAnchorForObject(ctx, upstream)
}

// ensureDownstreamZone mirrors upstream.Spec into a downstream shadow object idempotently.
func (r *DNSZoneReplicator) ensureDownstreamZone(ctx context.Context, strategy downstreamclient.ResourceStrategy, upstream *dnsv1alpha1.DNSZone) (controllerutil.OperationResult, error) {
	md, err := strategy.ObjectMetaFromUpstreamObject(ctx, upstream)
	if err != nil {
		return controllerutil.OperationResultNone, err
	}

	shadow := dnsv1alpha1.DNSZone{}
	shadow.SetGroupVersionKind(schema.GroupVersionKind{Group: "dns.networking.miloapis.com", Version: "v1alpha1", Kind: "DNSZone"})
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

// ensureSOARecordSet guarantees there is a managed SOA DNSRecordSet for PDNS-backed zones.
// It creates or patches a DNSRecordSet named "soa" in the same namespace that targets the zone root ("@")
// and uses the typed SOA fields with defaults derived from the zone name.
func (r *DNSZoneReplicator) ensureSOARecordSet(ctx context.Context, c client.Client, upstream *dnsv1alpha1.DNSZone) error {
	// Build desired SOA DNSRecordSet using nameservers from upstream status
	if len(upstream.Status.Nameservers) == 0 {
		return nil
	}
	mname := upstream.Status.Nameservers[0]
	if mname != "" && mname[len(mname)-1] != '.' {
		mname += "."
	}
	rname := "hostmaster." + upstream.Spec.DomainName + "."
	rsName := "soa"

	var existing dnsv1alpha1.DNSRecordSet
	if err := c.Get(ctx, client.ObjectKey{Namespace: upstream.Namespace, Name: rsName}, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			newObj := dnsv1alpha1.DNSRecordSet{}
			newObj.SetName(rsName)
			newObj.SetNamespace(upstream.Namespace)
			newObj.Spec = dnsv1alpha1.DNSRecordSetSpec{
				DNSZoneRef: corev1.LocalObjectReference{Name: upstream.Name},
				RecordType: dnsv1alpha1.RRTypeSOA,
				Records: []dnsv1alpha1.RecordEntry{{
					Name: "@",
					SOA: &dnsv1alpha1.SOARecordSpec{
						MName:   mname,
						RName:   rname,
						Refresh: 10800,
						Retry:   3600,
						Expire:  604800,
						TTL:     3600,
					},
				}},
			}
			return c.Create(ctx, &newObj)
		}
		return err
	}
	return nil
}

// ensureNSRecordSet ensures a root NS recordset reflecting nameservers from the upstream status.
func (r *DNSZoneReplicator) ensureNSRecordSet(ctx context.Context, c client.Client, upstream *dnsv1alpha1.DNSZone) error {
	if len(upstream.Status.Nameservers) == 0 {
		return nil
	}

	// Build desired NS DNSRecordSet at root from upstream status nameservers
	rsName := "ns"
	var existing dnsv1alpha1.DNSRecordSet
	if err := c.Get(ctx, client.ObjectKey{Namespace: upstream.Namespace, Name: rsName}, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			values := append([]string(nil), upstream.Status.Nameservers...)
			newObj := dnsv1alpha1.DNSRecordSet{}
			newObj.SetName(rsName)
			newObj.SetNamespace(upstream.Namespace)
			newObj.Spec = dnsv1alpha1.DNSRecordSetSpec{
				DNSZoneRef: corev1.LocalObjectReference{Name: upstream.Name},
				RecordType: dnsv1alpha1.RRTypeNS,
				Records: []dnsv1alpha1.RecordEntry{{
					Name: "@",
					Raw:  values,
				}},
			}
			return c.Create(ctx, &newObj)
		}
		return err
	}
	return nil
}

// updateStatus owns the upstream status synthesis: Accepted/Programmed, Nameservers.
func (r *DNSZoneReplicator) updateStatus(ctx context.Context, c client.Client, strategy downstreamclient.ResourceStrategy, upstream *dnsv1alpha1.DNSZone) error {
	changed := false

	// Nameservers: mirror from downstream DNSZone status
	if strategy != nil {
		if md, err := strategy.ObjectMetaFromUpstreamObject(ctx, upstream); err == nil {
			var shadow dnsv1alpha1.DNSZone
			shadow.SetNamespace(md.Namespace)
			shadow.SetName(md.Name)
			if err := r.DownstreamClient.Get(ctx, client.ObjectKey{Namespace: md.Namespace, Name: md.Name}, &shadow); err == nil {
				if !equality.Semantic.DeepEqual(upstream.Status.Nameservers, shadow.Status.Nameservers) {
					upstream.Status.Nameservers = append([]string(nil), shadow.Status.Nameservers...)
					changed = true
				}
			}
		}
	}

	// Accepted: true only after nameservers have been retrieved from downstream
	if len(upstream.Status.Nameservers) > 0 {
		changed = setCond(&upstream.Status.Conditions, CondAccepted, ReasonAccepted, "Nameservers retrieved from downstream", metav1.ConditionTrue, upstream.Generation) || changed
	} else {
		changed = setCond(&upstream.Status.Conditions, CondAccepted, ReasonPending, "Waiting for downstream nameservers", metav1.ConditionFalse, upstream.Generation) || changed
	}

	// Programmed: true only after default NS and SOA recordsets exist
	programmed := false
	if len(upstream.Status.Nameservers) > 0 {
		var rsSOA dnsv1alpha1.DNSRecordSet
		var rsNS dnsv1alpha1.DNSRecordSet
		errSOA := c.Get(ctx, client.ObjectKey{Namespace: upstream.Namespace, Name: "soa"}, &rsSOA)
		errNS := c.Get(ctx, client.ObjectKey{Namespace: upstream.Namespace, Name: "ns"}, &rsNS)
		if errSOA == nil && errNS == nil {
			programmed = true
		}
	}
	if programmed {
		changed = setCond(&upstream.Status.Conditions, CondProgrammed, ReasonProgrammed, "Default records ensured", metav1.ConditionTrue, upstream.Generation) || changed
	} else {
		changed = setCond(&upstream.Status.Conditions, CondProgrammed, ReasonPending, "Waiting for default records", metav1.ConditionFalse, upstream.Generation) || changed
	}

	// RecordCount: compute number of DNSRecordSets referencing this zone and set status field
	if rc, ok := r.computeRecordCount(ctx, c, upstream.Namespace, upstream.Name); ok {
		if upstream.Status.RecordCount != rc {
			upstream.Status.RecordCount = rc
			changed = true
		}
	}

	if !changed {
		return nil
	}
	if err := c.Status().Update(ctx, upstream); err != nil {
		return err
	}
	log.FromContext(ctx).Info("upstream zone status updated", "status", upstream.Status)
	return nil
}

// computeRecordCount lists DNSRecordSets in namespace and counts those that reference zoneName.
func (r *DNSZoneReplicator) computeRecordCount(ctx context.Context, c client.Client, namespace, zoneName string) (int, bool) {
	var rsList dnsv1alpha1.DNSRecordSetList
	if err := c.List(ctx, &rsList, client.InNamespace(namespace)); err != nil {
		return 0, false
	}
	count := 0
	for i := range rsList.Items {
		if rsList.Items[i].Spec.DNSZoneRef.Name == zoneName {
			count += len(rsList.Items[i].Spec.Records)
		}
	}
	return count, true
}

// ---- Watches / mapping helpers --------------------------------------------

func (r *DNSZoneReplicator) SetupWithManager(mgr mcmanager.Manager, downstreamCl cluster.Cluster) error {
	r.mgr = mgr

	b := mcbuilder.TypedControllerManagedBy[GVKRequest](mgr)

	// Upstream watch
	zoneGVK := schema.GroupVersionKind{Group: "dns.networking.miloapis.com", Version: "v1alpha1", Kind: "DNSZone"}
	upstreamZone := newUnstructuredForGVK(zoneGVK)
	b = b.Watches(upstreamZone, typedEnqueueRequestForGVK(zoneGVK))

	// Also watch upstream DNSRecordSet to keep RecordCount up-to-date on the zone
	rsGVK := schema.GroupVersionKind{Group: "dns.networking.miloapis.com", Version: "v1alpha1", Kind: "DNSRecordSet"}
	upstreamRS := newUnstructuredForGVK(rsGVK)
	b = b.Watches(upstreamRS, func(clusterName string, _ cluster.Cluster) handler.TypedEventHandler[client.Object, GVKRequest] {
		return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []GVKRequest {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return nil
			}
			// Read spec.dnsZoneRef.name from unstructured content
			m := u.UnstructuredContent()
			spec, _ := m["spec"].(map[string]interface{})
			if spec == nil {
				return nil
			}
			zref, _ := spec["dnsZoneRef"].(map[string]interface{})
			if zref == nil {
				return nil
			}
			zname, _ := zref["name"].(string)
			if zname == "" {
				return nil
			}
			return []GVKRequest{{
				GVK: zoneGVK,
				Request: mcreconcile.Request{
					ClusterName: clusterName,
					Request:     crreconcile.Request{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: zname}},
				},
			}}
		})
	})

	// Downstream watch (wake upstream on changes)
	downstreamZone := newUnstructuredForGVK(zoneGVK)
	src := mcsource.TypedKind(
		downstreamZone,
		typedEnqueueDownstreamGVKRequest(zoneGVK),
	)
	clusterSrc, err := src.ForCluster("", downstreamCl)
	if err != nil {
		return fmt.Errorf("failed to build downstream watch for %s: %w", zoneGVK.String(), err)
	}
	b = b.WatchesRawSource(clusterSrc)

	return b.Named("dnszone-replicator").Complete(r)
}
