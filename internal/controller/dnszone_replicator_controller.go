package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
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

type DNSZoneReplicator struct {
	mgr              mcmanager.Manager
	DownstreamClient client.Client
}

// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones,verbs=get;list;watch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch

// Reconcile flow:
// 1) Fetch upstream
// 2) Deletion guard (best-effort cleanup)
// 3) Ensure downstream shadow (mirror spec)
// 4) Synthesize upstream status (Accepted/Programmed + nameservers)
func (r *DNSZoneReplicator) Reconcile(ctx context.Context, req GVKRequest) (ctrl.Result, error) {
	lg := log.FromContext(ctx).WithValues("cluster", req.ClusterName, "namespace", req.Namespace, "name", req.Name)
	ctx = log.IntoContext(ctx, lg)
	lg.Info("reconcile start")

	upstreamCl, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if ok := upstreamCl.GetCache().WaitForCacheSync(ctx); !ok {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	// 1) Fetch upstream
	upstream, err := r.fetchUpstream(ctx, upstreamCl, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	strategy := downstreamclient.NewMappedNamespaceResourceStrategy(req.ClusterName, upstreamCl.GetClient(), r.DownstreamClient)

	// 2) Deletion guard
	if !upstream.DeletionTimestamp.IsZero() {
		if err := r.handleDeletion(ctx, strategy, &upstream); err != nil {
			lg.Error(err, "deleting anchor")
		}
		return ctrl.Result{}, nil
	}

	// 3) Ensure downstream shadow
	_, err = r.ensureShadow(ctx, strategy, &upstream)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 4) Ensure an upstream SOA DNSRecordSet exists for PDNS-backed zones
	if err := r.ensureSOARecordSet(ctx, upstreamCl.GetClient(), &upstream); err != nil {
		return ctrl.Result{}, err
	}

	// 5) Synthesize upstream status
	if err := r.updateStatus(ctx, upstreamCl.GetClient(), &upstream); err != nil {
		if !apierrors.IsNotFound(err) { // tolerate races
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

// ensureShadow mirrors upstream.Spec into a downstream shadow object idempotently.
func (r *DNSZoneReplicator) ensureShadow(ctx context.Context, strategy downstreamclient.ResourceStrategy, upstream *dnsv1alpha1.DNSZone) (controllerutil.OperationResult, error) {
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
	// Only when a DNSZoneClass is referenced and controllerName is powerdns
	if upstream.Spec.DNSZoneClassName == "" {
		return nil
	}
	var zc dnsv1alpha1.DNSZoneClass
	if err := c.Get(ctx, client.ObjectKey{Name: upstream.Spec.DNSZoneClassName}, &zc); err != nil {
		return client.IgnoreNotFound(err)
	}
	if zc.Spec.ControllerName != ControllerNamePowerDNS {
		return nil
	}

	// Build desired SOA DNSRecordSet
	mname := "ns1." + upstream.Spec.DomainName + "."
	rname := "hostmaster." + upstream.Spec.DomainName + "."
	rsName := "soa"

	var existing dnsv1alpha1.DNSRecordSet
	err := c.Get(ctx, client.ObjectKey{Namespace: upstream.Namespace, Name: rsName}, &existing)
	if err != nil {
		// Create when not found
		if apierrors.IsNotFound(err) {
			newObj := dnsv1alpha1.DNSRecordSet{}
			newObj.SetName(rsName)
			newObj.SetNamespace(upstream.Namespace)
			newObj.Spec = dnsv1alpha1.DNSRecordSetSpec{
				DNSZoneRef: corev1.LocalObjectReference{ // alias core type
					Name: upstream.Name,
				},
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
			// Controller reference not set here; upstream SOA is operator-managed but not owned to avoid GC surprises
			return c.Create(ctx, &newObj)
		}
		return err
	}

	// Patch when spec diverges
	desired := existing.DeepCopy()
	desired.Spec.RecordType = dnsv1alpha1.RRTypeSOA
	desired.Spec.DNSZoneRef = corev1.LocalObjectReference{Name: upstream.Name}
	desired.Spec.Records = []dnsv1alpha1.RecordEntry{{
		Name: "@",
		SOA: &dnsv1alpha1.SOARecordSpec{
			MName:   mname,
			RName:   rname,
			Refresh: 10800,
			Retry:   3600,
			Expire:  604800,
			TTL:     3600,
		},
	}}

	if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		return nil
	}
	existing.Spec = desired.Spec
	return c.Update(ctx, &existing)
}

// updateStatus owns the upstream status synthesis: Accepted/Programmed, Nameservers.
func (r *DNSZoneReplicator) updateStatus(ctx context.Context, c client.Client, upstream *dnsv1alpha1.DNSZone) error {
	// Accepted: true once downstream ensured (we reached here)
	changed := setCond(&upstream.Status.Conditions, CondAccepted, ReasonAccepted, "Downstream shadow ensured", metav1.ConditionTrue, upstream.Generation)

	// Programmed: assumed true for now (TODO: verify via external SOA when available)
	changed = setCond(&upstream.Status.Conditions, CondProgrammed, ReasonProgrammed, "Assumed programmed (TODO verify)", metav1.ConditionTrue, upstream.Generation) || changed

	// Nameservers: derive from DNSZoneClass if referenced
	if ns, ok := r.deriveNameServers(ctx, c, upstream.Spec.DNSZoneClassName); ok {
		if !equality.Semantic.DeepEqual(upstream.Status.Nameservers, ns) {
			upstream.Status.Nameservers = append([]string(nil), ns...)
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

func (r *DNSZoneReplicator) deriveNameServers(ctx context.Context, c client.Client, className string) ([]string, bool) {
	if className == "" {
		return nil, false
	}
	var zc dnsv1alpha1.DNSZoneClass
	if err := c.Get(ctx, client.ObjectKey{Name: className}, &zc); err != nil {
		return nil, false
	}
	if zc.Spec.NameServerPolicy.Mode != dnsv1alpha1.NameServerPolicyModeStatic || zc.Spec.NameServerPolicy.Static == nil {
		return nil, false
	}
	// copy to avoid aliasing
	out := append([]string(nil), zc.Spec.NameServerPolicy.Static.Servers...)
	return out, true
}

// ---- Watches / mapping helpers --------------------------------------------

func (r *DNSZoneReplicator) SetupWithManager(mgr mcmanager.Manager, downstreamCl cluster.Cluster) error {
	r.mgr = mgr

	b := mcbuilder.TypedControllerManagedBy[GVKRequest](mgr)

	// Upstream watch
	zoneGVK := schema.GroupVersionKind{Group: "dns.networking.miloapis.com", Version: "v1alpha1", Kind: "DNSZone"}
	upstreamZone := newUnstructuredForGVK(zoneGVK)
	b = b.Watches(upstreamZone, typedEnqueueRequestForGVK(zoneGVK))

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
