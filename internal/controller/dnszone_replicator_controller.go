package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
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

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	downstreamclient "go.miloapis.com/dns-operator/internal/downstreamclient"
)

type DNSZoneReplicator struct {
	mgr              mcmanager.Manager
	DownstreamClient client.Client
}

const dnsZoneFinalizer = "dns.networking.miloapis.com/finalize-dnszone"

// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones/finalizers,verbs=update;patch;delete
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=domains,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=domains/status,verbs=get;list;watch
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
		if !controllerutil.ContainsFinalizer(&upstream, dnsZoneFinalizer) {
			base := upstream.DeepCopy()
			upstream.Finalizers = append(upstream.Finalizers, dnsZoneFinalizer)
			if err := upstreamCl.GetClient().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
				lg.Error(err, "failed to add upstream finalizer")
				return ctrl.Result{RequeueAfter: 300 * time.Millisecond}, nil
			}
			// Re-run with updated object
			return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
		}
	} else {
		// Deletion guard: ensure downstream is gone before removing finalizer
		if controllerutil.ContainsFinalizer(&upstream, dnsZoneFinalizer) {
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
			controllerutil.RemoveFinalizer(&upstream, dnsZoneFinalizer)
			if err := upstreamCl.GetClient().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
				lg.Error(err, "failed to remove upstream finalizer")
				return ctrl.Result{RequeueAfter: 300 * time.Millisecond}, nil
			}
		}
		return ctrl.Result{}, nil
	}

	// If DNSZoneClassName is not set, set Accepted=False with reason
	if upstream.Spec.DNSZoneClassName == "" {
		base := upstream.DeepCopy() // take snapshot BEFORE mutating status
		if setCond(&upstream.Status.Conditions, CondAccepted, ReasonPending, "DNSZoneClassName not set", metav1.ConditionFalse, upstream.Generation) {
			if err := upstreamCl.GetClient().Status().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	strategy := downstreamclient.NewMappedNamespaceResourceStrategy(req.ClusterName, upstreamCl.GetClient(), r.DownstreamClient)
	// 2) Resolve DNSZoneClass early; if specified but not found, set Accepted=False with message and stop
	var zoneClass dnsv1alpha1.DNSZoneClass
	if err := upstreamCl.GetClient().Get(ctx, client.ObjectKey{Name: upstream.Spec.DNSZoneClassName}, &zoneClass); err != nil {
		if apierrors.IsNotFound(err) {
			base := upstream.DeepCopy()
			if setCond(&upstream.Status.Conditions, CondAccepted, ReasonPending,
				fmt.Sprintf("DNSZoneClass %q not found", upstream.Spec.DNSZoneClassName),
				metav1.ConditionFalse, upstream.Generation) {
				if err := upstreamCl.GetClient().Status().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 3) Ensure downstream shadow (only after class presence check)
	_, err = r.ensureDownstreamZone(ctx, strategy, &upstream)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Ensure a matching Domain exists in the upstream cluster for this zone's domain name.
	if err := r.ensureDomain(ctx, upstreamCl.GetClient(), &upstream); err != nil {
		return ctrl.Result{}, err
	}

	// 4) First, refresh upstream status from downstream (populate nameservers)
	if err := r.updateStatus(ctx, upstreamCl.GetClient(), strategy, &upstream); err != nil {
		if !apierrors.IsNotFound(err) { // tolerate races
			return ctrl.Result{}, err
		}
	}

	// If nameservers are not yet present, lightly poll to catch downstream status changes
	if len(upstream.Status.Nameservers) == 0 {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// 5) Ensure default records exist (nameservers known by guard above)
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

	// Idempotency via labels: if an SOA recordset for this zone already exists, do nothing
	var existingList dnsv1alpha1.DNSRecordSetList
	if err := c.List(
		ctx,
		&existingList,
		client.InNamespace(upstream.Namespace),
		client.MatchingFieldsSelector{Selector: fields.AndSelectors(
			fields.OneTermEqualSelector("spec.dnsZoneRef.name", upstream.Name),
			fields.OneTermEqualSelector("spec.recordType", string(dnsv1alpha1.RRTypeSOA)),
		)},
	); err != nil {
		return err
	}
	if len(existingList.Items) > 0 {
		return nil
	}

	newObj := dnsv1alpha1.DNSRecordSet{}
	newObj.SetNamespace(upstream.Namespace)
	newObj.SetGenerateName(rsName + "-")
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

// ensureNSRecordSet ensures a root NS recordset reflecting nameservers from the upstream status.
func (r *DNSZoneReplicator) ensureNSRecordSet(ctx context.Context, c client.Client, upstream *dnsv1alpha1.DNSZone) error {
	if len(upstream.Status.Nameservers) == 0 {
		return nil
	}

	// Build desired NS DNSRecordSet at root from upstream status nameservers
	rsName := "ns"
	// Idempotency via labels: if an NS recordset for this zone already exists, do nothing
	var existingList dnsv1alpha1.DNSRecordSetList
	if err := c.List(
		ctx,
		&existingList,
		client.InNamespace(upstream.Namespace),
		client.MatchingFieldsSelector{Selector: fields.AndSelectors(
			fields.OneTermEqualSelector("spec.dnsZoneRef.name", upstream.Name),
			fields.OneTermEqualSelector("spec.recordType", string(dnsv1alpha1.RRTypeNS)),
		)},
	); err != nil {
		return err
	}
	if len(existingList.Items) > 0 {
		return nil
	}

	values := append([]string(nil), upstream.Status.Nameservers...)
	newObj := dnsv1alpha1.DNSRecordSet{}
	newObj.SetNamespace(upstream.Namespace)
	newObj.SetGenerateName(rsName + "-")
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

// updateStatus owns the upstream status synthesis: Accepted/Programmed, Nameservers.
func (r *DNSZoneReplicator) updateStatus(ctx context.Context, c client.Client, strategy downstreamclient.ResourceStrategy, upstream *dnsv1alpha1.DNSZone) error {
	base := upstream.DeepCopy()
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

	// DomainRef: populate from upstream Domain object that matches spec.domainName.
	var dlist networkingv1alpha.DomainList
	if err := c.List(
		ctx,
		&dlist,
		client.InNamespace(upstream.Namespace),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("spec.domainName", upstream.Spec.DomainName)},
	); err != nil {
		return err
	}
	var newRef *dnsv1alpha1.DomainRef
	if len(dlist.Items) > 0 {
		// Try to find one thats verified first else pick the first one.
		indx := 0
		for i, d := range dlist.Items {
			if apimeta.IsStatusConditionTrue(d.Status.Conditions, networkingv1alpha.DomainConditionVerified) {
				indx = i
				break
			}
		}

		d := dlist.Items[indx]
		newRef = &dnsv1alpha1.DomainRef{
			Name: d.Name,
			Status: dnsv1alpha1.DomainRefStatus{
				Nameservers: append([]networkingv1alpha.Nameserver(nil), d.Status.Nameservers...),
			},
		}
	}
	if !equality.Semantic.DeepEqual(upstream.Status.DomainRef, newRef) {
		upstream.Status.DomainRef = newRef
		changed = true
	}

	// Accepted: true only after nameservers have been retrieved from downstream
	if len(upstream.Status.Nameservers) > 0 {
		changed = setCond(&upstream.Status.Conditions, CondAccepted, ReasonAccepted, "Nameservers retrieved from downstream", metav1.ConditionTrue, upstream.Generation) || changed
	} else {
		changed = setCond(&upstream.Status.Conditions, CondAccepted, ReasonPending, "Waiting for downstream nameservers", metav1.ConditionFalse, upstream.Generation) || changed
	}

	var rsList dnsv1alpha1.DNSRecordSetList
	if err := c.List(
		ctx,
		&rsList,
		client.InNamespace(upstream.Namespace),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("spec.dnsZoneRef.name", upstream.Name)},
	); err != nil {
		return err
	}

	// Programmed: true only after default NS and SOA recordsets exist
	programmed := false
	if len(upstream.Status.Nameservers) > 0 {
		haveSOA := false
		haveNS := false
		for i := range rsList.Items {
			if rsList.Items[i].Spec.RecordType == dnsv1alpha1.RRTypeSOA {
				haveSOA = true
			}
			if rsList.Items[i].Spec.RecordType == dnsv1alpha1.RRTypeNS {
				haveNS = true
			}
			if haveSOA && haveNS {
				break
			}
		}
		programmed = haveSOA && haveNS

	}
	if programmed {
		changed = setCond(&upstream.Status.Conditions, CondProgrammed, ReasonProgrammed, "Default records ensured", metav1.ConditionTrue, upstream.Generation) || changed
	} else {
		changed = setCond(&upstream.Status.Conditions, CondProgrammed, ReasonPending, "Waiting for default records", metav1.ConditionFalse, upstream.Generation) || changed
	}

	// RecordCount: compute number of DNSRecordSets referencing this zone and set status field
	recordCount := 0
	for i := range rsList.Items {
		recordCount += len(rsList.Items[i].Spec.Records)
	}
	if upstream.Status.RecordCount != recordCount {
		upstream.Status.RecordCount = recordCount
		changed = true
	}

	if !changed {
		return nil
	}
	if err := c.Status().Patch(ctx, upstream, client.MergeFrom(base)); err != nil {
		return err
	}
	log.FromContext(ctx).Info("upstream zone status updated", "status", upstream.Status)
	return nil
}

// ensureDomain guarantees that a Domain object with spec.domainName equal to the zone's domain name exists upstream.
// If none exists, it creates one in the same namespace.
func (r *DNSZoneReplicator) ensureDomain(ctx context.Context, c client.Client, upstream *dnsv1alpha1.DNSZone) error {
	var existing networkingv1alpha.DomainList
	if err := c.List(
		ctx,
		&existing,
		client.InNamespace(upstream.Namespace),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("spec.domainName", upstream.Spec.DomainName)},
	); err != nil {
		return err
	}
	if len(existing.Items) > 0 {
		return nil
	}
	newDomain := networkingv1alpha.Domain{}
	newDomain.SetNamespace(upstream.Namespace)
	newDomain.SetGenerateName(fmt.Sprintf("%s-", strings.ReplaceAll(upstream.Spec.DomainName, ".", "-")))
	newDomain.Spec.DomainName = upstream.Spec.DomainName
	return c.Create(ctx, &newDomain)
}

// ---- Watches / mapping helpers --------------------------------------------

func (r *DNSZoneReplicator) SetupWithManager(mgr mcmanager.Manager, downstreamCl cluster.Cluster) error {
	r.mgr = mgr

	// Register field indexes used by this controller when listing DNSRecordSets
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&dnsv1alpha1.DNSRecordSet{}, "spec.dnsZoneRef.name",
		func(obj client.Object) []string {
			rs := obj.(*dnsv1alpha1.DNSRecordSet)
			return []string{rs.Spec.DNSZoneRef.Name}
		},
	); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&dnsv1alpha1.DNSRecordSet{}, "spec.recordType",
		func(obj client.Object) []string {
			rs := obj.(*dnsv1alpha1.DNSRecordSet)
			return []string{string(rs.Spec.RecordType)}
		},
	); err != nil {
		return err
	}
	// Index DNSZone by spec.domainName to efficiently map Domains -> Zones across namespaces.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&dnsv1alpha1.DNSZone{}, "spec.domainName",
		func(obj client.Object) []string {
			z := obj.(*dnsv1alpha1.DNSZone)
			return []string{z.Spec.DomainName}
		},
	); err != nil {
		return err
	}
	// Index for Domain.spec.domainName to efficiently lookups by domain name.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&networkingv1alpha.Domain{}, "spec.domainName",
		func(obj client.Object) []string {
			d := obj.(*networkingv1alpha.Domain)
			return []string{d.Spec.DomainName}
		},
	); err != nil {
		return err
	}

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

	// Watch upstream Domain objects and enqueue only if a matching DNSZone exists.
	domainGVK := schema.GroupVersionKind{Group: networkingv1alpha.GroupVersion.Group, Version: networkingv1alpha.GroupVersion.Version, Kind: "Domain"}
	upstreamDomain := newUnstructuredForGVK(domainGVK)
	b = b.Watches(upstreamDomain, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[client.Object, GVKRequest] {
		return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []GVKRequest {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return nil
			}
			m := u.UnstructuredContent()
			spec, _ := m["spec"].(map[string]interface{})
			if spec == nil {
				return nil
			}
			dname, _ := spec["domainName"].(string)
			if dname == "" {
				return nil
			}
			// Find upstream DNSZone(s) in the same namespace as the Domain.
			var zones dnsv1alpha1.DNSZoneList
			if err := cl.GetClient().List(ctx, &zones,
				client.InNamespace(obj.GetNamespace()),
				client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("spec.domainName", dname)},
			); err != nil {
				return nil
			}
			// TODO: ideally there is only ever one DNSZone with the same spec.domainName in the same namespace.
			var reqs []GVKRequest
			for i := range zones.Items {
				reqs = append(reqs, GVKRequest{
					GVK: zoneGVK,
					Request: mcreconcile.Request{
						ClusterName: clusterName,
						Request:     crreconcile.Request{NamespacedName: types.NamespacedName{Namespace: zones.Items[i].Namespace, Name: zones.Items[i].Name}},
					},
				})
			}
			return reqs
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
