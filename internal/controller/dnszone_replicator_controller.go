package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
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
	// AccountingNamespace is the downstream namespace used for DNSZone ownership accounting.
	AccountingNamespace string
}

const (
	dnsZoneFinalizer               = "dns.networking.miloapis.com/finalize-dnszone"
	verificationRecordRequeueDelay = 1 * time.Second
)

// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones/finalizers,verbs=update;patch;delete
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=domains,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.datumapis.com,resources=domains/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *DNSZoneReplicator) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
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
				return ctrl.Result{}, err
			}
			lg.Info("added upstream finalizer", "finalizer", dnsZoneFinalizer)
			// Re-run with updated object
			return ctrl.Result{}, nil
		}
	} else {
		// Deletion guard: ensure downstream is gone before removing finalizer
		if controllerutil.ContainsFinalizer(&upstream, dnsZoneFinalizer) {
			strategy := downstreamclient.NewMappedNamespaceResourceStrategy(req.ClusterName, upstreamCl.GetClient(), r.DownstreamClient)

			// Request deletion of downstream anchor/shadow
			if err := r.handleDeletion(ctx, strategy, &upstream); err != nil && !apierrors.IsNotFound(err) {
				lg.Error(err, "downstream delete failed; will retry")
				return ctrl.Result{}, err
			}
			lg.Info("requested downstream delete for DNSZone")

			// Verify downstream object is actually gone before dropping finalizer
			md, mdErr := strategy.ObjectMetaFromUpstreamObject(ctx, &upstream)
			if mdErr != nil {
				lg.Error(mdErr, "failed to compute downstream metadata; will retry")
				return ctrl.Result{}, err
			}
			var shadow dnsv1alpha1.DNSZone
			shadow.SetNamespace(md.Namespace)
			shadow.SetName(md.Name)
			getErr := r.DownstreamClient.Get(ctx, client.ObjectKey{Namespace: md.Namespace, Name: md.Name}, &shadow)
			if getErr == nil {
				lg.Info("downstream DNSZone still exists; waiting", "namespace", md.Namespace, "name", md.Name)
				return ctrl.Result{}, nil
			}
			if !apierrors.IsNotFound(getErr) {
				// Transient error; retry
				lg.Error(getErr, "failed to check downstream deletion; will retry")
				return ctrl.Result{}, err
			}

			// Cleanup zone accounting configmap once downstream is confirmed gone
			lg.Info("downstream DNSZone confirmed deleted", "namespace", md.Namespace, "name", md.Name)
			owner := fmt.Sprintf("%s/%s/%s", req.ClusterName, upstream.Namespace, upstream.Name)
			if cerr := r.cleanupZoneAccounting(ctx, &upstream, owner); cerr != nil && !apierrors.IsNotFound(cerr) {
				lg.Error(cerr, "failed to cleanup accounting configmap; will retry")
				return ctrl.Result{}, err
			}
			lg.Info("cleaned up accounting configmap for DNSZone", "domain", upstream.Spec.DomainName)

			// Downstream is confirmed gone -> remove upstream finalizer (conflict-safe)
			base := upstream.DeepCopy()
			controllerutil.RemoveFinalizer(&upstream, dnsZoneFinalizer)
			if err := upstreamCl.GetClient().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
				lg.Error(err, "failed to remove upstream finalizer")
				return ctrl.Result{}, err
			}
			lg.Info("removed upstream finalizer", "finalizer", dnsZoneFinalizer)
		}
		return ctrl.Result{}, nil
	}

	// If DNSZoneClassName is not set, set Accepted=False with reason
	if upstream.Spec.DNSZoneClassName == "" {
		base := upstream.DeepCopy() // take snapshot BEFORE mutating status
		if apimeta.SetStatusCondition(&upstream.Status.Conditions, metav1.Condition{
			Type:               CondAccepted,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonPending,
			Message:            "DNSZoneClassName not set",
			ObservedGeneration: upstream.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		}) {
			if err := upstreamCl.GetClient().Status().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
		}
		lg.Info("DNSZoneClassName not set; marked Accepted=False and exiting early")
		return ctrl.Result{}, nil
	}

	strategy := downstreamclient.NewMappedNamespaceResourceStrategy(req.ClusterName, upstreamCl.GetClient(), r.DownstreamClient)
	// 2) Resolve DNSZoneClass early; if specified but not found, set Accepted=False with message and stop
	var zoneClass dnsv1alpha1.DNSZoneClass
	if err := upstreamCl.GetClient().Get(ctx, client.ObjectKey{Name: upstream.Spec.DNSZoneClassName}, &zoneClass); err != nil {
		if apierrors.IsNotFound(err) {
			base := upstream.DeepCopy()
			if apimeta.SetStatusCondition(&upstream.Status.Conditions, metav1.Condition{
				Type:               CondAccepted,
				Status:             metav1.ConditionFalse,
				Reason:             ReasonPending,
				Message:            fmt.Sprintf("DNSZoneClass %q not found", upstream.Spec.DNSZoneClassName),
				ObservedGeneration: upstream.Generation,
				LastTransitionTime: metav1.NewTime(time.Now()),
			}) {
				if err := upstreamCl.GetClient().Status().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
					return ctrl.Result{}, err
				}
			}
			lg.Info("DNSZoneClass not found; marked Accepted=False and exiting early", "class", upstream.Spec.DNSZoneClassName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 3) Ensure downstream shadow (only after class presence check)
	owner := fmt.Sprintf("%s/%s/%s", req.ClusterName, upstream.Namespace, upstream.Name)
	if owned, aerr := r.ensureZoneAccounting(ctx, &upstream, owner); aerr != nil {
		return ctrl.Result{}, aerr
	} else if !owned {
		base := upstream.DeepCopy()
		changed := false
		if apimeta.SetStatusCondition(&upstream.Status.Conditions, metav1.Condition{
			Type:               CondAccepted,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonDNSZoneInUse,
			Message:            "DNSZone claimed by another resource",
			ObservedGeneration: upstream.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		}) {
			changed = true
		}
		if apimeta.SetStatusCondition(&upstream.Status.Conditions, metav1.Condition{
			Type:               CondProgrammed,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonDNSZoneInUse,
			Message:            "DNSZone claimed by another resource",
			ObservedGeneration: upstream.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		}) {
			changed = true
		}
		if changed {
			if perr := upstreamCl.GetClient().Status().Patch(ctx, &upstream, client.MergeFrom(base)); perr != nil {
				return ctrl.Result{}, perr
			}
		}
		return ctrl.Result{}, nil
	}
	if _, err = r.ensureDownstreamZone(ctx, strategy, &upstream); err != nil {
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

	// If nameservers are not yet present, rely on downstream watch to trigger requeue
	if len(upstream.Status.Nameservers) == 0 {
		lg.Info("nameservers not yet available; waiting for downstream update")
		return ctrl.Result{}, nil
	}

	// 5) Ensure default records exist (nameservers known by guard above)
	if err := r.ensureSOARecordSet(ctx, upstreamCl.GetClient(), &upstream); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureNSRecordSet(ctx, upstreamCl.GetClient(), &upstream); err != nil {
		return ctrl.Result{}, err
	}
	pendingVerificationTXT, err := r.ensureTXTRecordSet(ctx, upstreamCl.GetClient(), &upstream)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Recompute status to set Programmed based on record presence
	if err := r.updateStatus(ctx, upstreamCl.GetClient(), strategy, &upstream); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	if pendingVerificationTXT {
		return ctrl.Result{RequeueAfter: verificationRecordRequeueDelay}, nil
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
	shadow.SetNamespace(md.Namespace)
	shadow.SetName(md.Name)

	res, cErr := controllerutil.CreateOrPatch(ctx, strategy.GetClient(), &shadow, func() error {
		shadow.Labels = md.Labels
		if !equality.Semantic.DeepEqual(shadow.Spec, upstream.Spec) {
			shadow.Spec = upstream.Spec
		}
		return strategy.SetControllerReference(ctx, upstream, &shadow)
	})
	if cErr != nil {
		return res, cErr
	}
	log.FromContext(ctx).Info("ensured downstream DNSZone", "operation", res, "namespace", shadow.Namespace, "name", shadow.Name)
	return res, nil
}

// ensureZoneAccounting ensures a ConfigMap exists in the accounting namespace keyed by domainName,
// and that its Data["owner"] matches the provided owner value. It creates the namespace/configmap if needed.
// Returns owned=true when this zone owns the ConfigMap; owned=false if another owner holds it.
func (r *DNSZoneReplicator) ensureZoneAccounting(ctx context.Context, upstream *dnsv1alpha1.DNSZone, owner string) (bool, error) {
	ns := r.AccountingNamespace
	if ns == "" {
		// This should never happen, but if it does, we should log an error and return an error.
		log.FromContext(ctx).Error(fmt.Errorf("accounting namespace is not set"), "ensureZoneAccounting")
		return false, fmt.Errorf("accounting namespace is not set")
	}
	// Ensure namespace exists
	var namespace corev1.Namespace
	if err := r.DownstreamClient.Get(ctx, client.ObjectKey{Name: ns}, &namespace); err != nil {
		if apierrors.IsNotFound(err) {
			namespace = corev1.Namespace{}
			namespace.Name = ns
			if cerr := r.DownstreamClient.Create(ctx, &namespace); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
				return false, cerr
			}
			log.FromContext(ctx).Info("created accounting namespace (downstream)", "namespace", ns)
		} else {
			return false, err
		}
	}

	var cm corev1.ConfigMap
	if err := r.DownstreamClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: upstream.Spec.DomainName}, &cm); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, err
		}
		// Create new ownership CM
		newCM := corev1.ConfigMap{}
		newCM.Namespace = ns
		newCM.Name = upstream.Spec.DomainName
		newCM.Data = map[string]string{
			"owner": owner,
		}
		if cerr := r.DownstreamClient.Create(ctx, &newCM); cerr != nil {
			// A race can occur; if created by another, treat as not owned and let next reconcile decide
			if apierrors.IsAlreadyExists(cerr) {
				// Re-fetch and compare
				if gerr := r.DownstreamClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: upstream.Spec.DomainName}, &cm); gerr == nil {
					owned := cm.Data["owner"] == owner
					log.FromContext(ctx).Info("zone accounting exists after race", "namespace", ns, "configmap", upstream.Spec.DomainName, "owned", owned, "owner", cm.Data["owner"])
					return owned, nil
				}
			}
			return false, cerr
		}
		log.FromContext(ctx).Info("created zone accounting configmap (downstream)", "namespace", ns, "configmap", newCM.Name, "owner", owner)
		return true, nil
	}

	// Exists: check ownership
	owned := cm.Data["owner"] == owner
	log.FromContext(ctx).Info("zone accounting found (downstream)", "namespace", ns, "configmap", cm.Name, "owned", owned, "owner", cm.Data["owner"])
	return owned, nil
}

// cleanupZoneAccounting deletes the ownership ConfigMap for a zone if it is owned by the provided owner.
func (r *DNSZoneReplicator) cleanupZoneAccounting(ctx context.Context, upstream *dnsv1alpha1.DNSZone, owner string) error {
	ns := r.AccountingNamespace
	if ns == "" {
		// This should never happen, but if it does, we should log an error and return an error.
		log.FromContext(ctx).Error(fmt.Errorf("accounting namespace is not set"), "cleanupZoneAccounting")
		return fmt.Errorf("accounting namespace is not set")
	}
	var cm corev1.ConfigMap
	if err := r.DownstreamClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: upstream.Spec.DomainName}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if cm.Data["owner"] != owner {
		// Do not delete if not owned by us
		log.FromContext(ctx).Info("skipping accounting configmap delete; not owner", "namespace", ns, "configmap", cm.Name, "owner", cm.Data["owner"], "expectedOwner", owner)
		return nil
	}
	if err := r.DownstreamClient.Delete(ctx, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	log.FromContext(ctx).Info("deleted accounting configmap (downstream)", "namespace", ns, "configmap", cm.Name)
	return nil
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

	// if an SOA recordset for this zone already exists, do nothing
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
		log.FromContext(ctx).Info("SOA DNSRecordSet already present; skipping create", "namespace", upstream.Namespace, "dnsZone", upstream.Name)
		return nil
	}

	newObj := dnsv1alpha1.DNSRecordSet{}
	newObj.SetNamespace(upstream.Namespace)
	newObj.SetName(fmt.Sprintf("%s-%s", upstream.Name, rsName))
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
	log.FromContext(ctx).Info("creating default SOA DNSRecordSet (upstream)", "namespace", newObj.Namespace, "dnsZone", upstream.Name)
	return c.Create(ctx, &newObj)
}

// ensureNSRecordSet ensures a root NS recordset reflecting nameservers from the upstream status.
func (r *DNSZoneReplicator) ensureNSRecordSet(ctx context.Context, c client.Client, upstream *dnsv1alpha1.DNSZone) error {
	if len(upstream.Status.Nameservers) == 0 {
		return nil
	}

	// Build desired NS DNSRecordSet at root from upstream status nameservers
	rsName := "ns"
	// if an NS recordset for this zone already exists, do nothing
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
		log.FromContext(ctx).Info("NS DNSRecordSet already present; skipping create", "namespace", upstream.Namespace, "dnsZone", upstream.Name)
		return nil
	}

	records := make([]dnsv1alpha1.RecordEntry, 0, len(upstream.Status.Nameservers))
	for _, value := range upstream.Status.Nameservers {
		records = append(records, dnsv1alpha1.RecordEntry{
			Name: "@",
			NS: &dnsv1alpha1.NSRecordSpec{
				Content: value,
			},
		})
	}
	newObj := dnsv1alpha1.DNSRecordSet{}
	newObj.SetNamespace(upstream.Namespace)
	newObj.SetName(fmt.Sprintf("%s-%s", upstream.Name, rsName))
	newObj.Spec = dnsv1alpha1.DNSRecordSetSpec{
		DNSZoneRef: corev1.LocalObjectReference{Name: upstream.Name},
		RecordType: dnsv1alpha1.RRTypeNS,
		Records:    records,
	}
	log.FromContext(ctx).Info("creating default NS DNSRecordSet (upstream)", "namespace", newObj.Namespace, "dnsZone", upstream.Name)
	return c.Create(ctx, &newObj)
}

// ensureTXTRecordSet programs the domain verification TXT record once available on the Domain status.
// Returns requeue=true when verification data is not yet present so we can poll shortly.
func (r *DNSZoneReplicator) ensureTXTRecordSet(ctx context.Context, c client.Client, upstream *dnsv1alpha1.DNSZone) (bool, error) {
	lg := log.FromContext(ctx)

	// Already ensured; nothing to do.
	if apimeta.IsStatusConditionTrue(upstream.Status.Conditions, CondVerificationRecord) {
		return false, nil
	}

	base := upstream.DeepCopy()
	changed := false

	var domains networkingv1alpha.DomainList
	if err := c.List(
		ctx,
		&domains,
		client.InNamespace(upstream.Namespace),
		client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("spec.domainName", upstream.Spec.DomainName)},
	); err != nil {
		return false, err
	}

	if len(domains.Items) == 0 {
		if apimeta.SetStatusCondition(&upstream.Status.Conditions, metav1.Condition{
			Type:               CondVerificationRecord,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonVerificationRecordPending,
			Message:            "Waiting for Domain to exist before programming verification TXT",
			ObservedGeneration: upstream.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		}) {
			changed = true
		}
		if changed {
			if err := c.Status().Patch(ctx, upstream, client.MergeFrom(base)); err != nil {
				return false, err
			}
		}
		return true, nil
	}

	var chosen *networkingv1alpha.Domain
	for i := range domains.Items {
		d := &domains.Items[i]
		if apimeta.IsStatusConditionTrue(d.Status.Conditions, networkingv1alpha.DomainConditionVerified) {
			chosen = d
			break
		}
		if chosen == nil || d.CreationTimestamp.Before(&chosen.CreationTimestamp) {
			chosen = d
		}
	}

	if chosen == nil {
		if apimeta.SetStatusCondition(&upstream.Status.Conditions, metav1.Condition{
			Type:               CondVerificationRecord,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonVerificationRecordPending,
			Message:            "Domain lookup returned no candidates",
			ObservedGeneration: upstream.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		}) {
			changed = true
		}
		if changed {
			if err := c.Status().Patch(ctx, upstream, client.MergeFrom(base)); err != nil {
				return false, err
			}
		}
		return true, nil
	}

	if apimeta.IsStatusConditionTrue(chosen.Status.Conditions, networkingv1alpha.DomainConditionVerified) {
		if apimeta.SetStatusCondition(&upstream.Status.Conditions, metav1.Condition{
			Type:               CondVerificationRecord,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonVerificationDomainVerified,
			Message:            fmt.Sprintf("Domain %q already verified", chosen.Name),
			ObservedGeneration: upstream.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		}) {
			changed = true
		}
		if changed {
			if err := c.Status().Patch(ctx, upstream, client.MergeFrom(base)); err != nil {
				return false, err
			}
		}
		return false, nil
	}

	verification := chosen.Status.Verification
	if verification == nil || verification.DNSRecord.Name == "" || verification.DNSRecord.Content == "" {
		if apimeta.SetStatusCondition(&upstream.Status.Conditions, metav1.Condition{
			Type:               CondVerificationRecord,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonVerificationRecordPending,
			Message:            fmt.Sprintf("Waiting for verification TXT on Domain %q", chosen.Name),
			ObservedGeneration: upstream.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		}) {
			changed = true
		}
		if changed {
			if err := c.Status().Patch(ctx, upstream, client.MergeFrom(base)); err != nil {
				return false, err
			}
		}
		return true, nil
	}

	rsName := "txt"
	obj := dnsv1alpha1.DNSRecordSet{}
	obj.SetNamespace(upstream.Namespace)
	obj.SetName(fmt.Sprintf("%s-%s", upstream.Name, rsName))

	res, err := controllerutil.CreateOrPatch(ctx, c, &obj, func() error {
		obj.Spec.DNSZoneRef = corev1.LocalObjectReference{Name: upstream.Name}
		obj.Spec.RecordType = dnsv1alpha1.RRTypeTXT
		obj.Spec.Records = []dnsv1alpha1.RecordEntry{{
			Name: verification.DNSRecord.Name,
			TXT:  &dnsv1alpha1.TXTRecordSpec{Content: verification.DNSRecord.Content},
		}}
		return nil
	})
	if err != nil {
		return false, err
	}
	if res != controllerutil.OperationResultNone {
		lg.Info("ensured verification TXT DNSRecordSet", "operation", res, "namespace", obj.Namespace, "name", obj.Name)
	}

	if apimeta.SetStatusCondition(&upstream.Status.Conditions, metav1.Condition{
		Type:               CondVerificationRecord,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonVerificationRecordProgrammed,
		Message:            fmt.Sprintf("Verification TXT ensured from Domain %q", chosen.Name),
		ObservedGeneration: upstream.Generation,
		LastTransitionTime: metav1.NewTime(time.Now()),
	}) {
		changed = true
	}

	if changed {
		if err := c.Status().Patch(ctx, upstream, client.MergeFrom(base)); err != nil {
			return false, err
		}
	}

	return false, nil
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
		if apimeta.SetStatusCondition(&upstream.Status.Conditions, metav1.Condition{
			Type:               CondAccepted,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonAccepted,
			Message:            "Nameservers retrieved from downstream",
			ObservedGeneration: upstream.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		}) {
			changed = true
		}
	} else {
		if apimeta.SetStatusCondition(&upstream.Status.Conditions, metav1.Condition{
			Type:               CondAccepted,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonPending,
			Message:            "Waiting for downstream nameservers",
			ObservedGeneration: upstream.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		}) {
			changed = true
		}
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
		if apimeta.SetStatusCondition(&upstream.Status.Conditions, metav1.Condition{
			Type:               CondProgrammed,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonProgrammed,
			Message:            "Default records ensured",
			ObservedGeneration: upstream.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		}) {
			changed = true
		}
	} else {
		if apimeta.SetStatusCondition(&upstream.Status.Conditions, metav1.Condition{
			Type:               CondProgrammed,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonPending,
			Message:            "Waiting for default records",
			ObservedGeneration: upstream.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		}) {
			changed = true
		}
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
		log.FromContext(ctx).Info("Domain already exists for DNSZone; skipping create", "namespace", upstream.Namespace, "domainName", upstream.Spec.DomainName)
		return nil
	}
	newDomain := networkingv1alpha.Domain{}
	newDomain.SetNamespace(upstream.Namespace)
	newDomain.SetName(upstream.Spec.DomainName)
	newDomain.Spec.DomainName = upstream.Spec.DomainName
	log.FromContext(ctx).Info("creating Domain for DNSZone (upstream)", "namespace", newDomain.Namespace, "domainName", newDomain.Spec.DomainName)
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

	b := mcbuilder.ControllerManagedBy(mgr)

	// Upstream watch
	b = b.For(&dnsv1alpha1.DNSZone{}).Owns(&dnsv1alpha1.DNSRecordSet{})

	// Watch upstream Domain objects and enqueue only if a matching DNSZone exists.
	b = b.Watches(&networkingv1alpha.Domain{}, func(clusterName string, cl cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
		return handler.TypedEnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []mcreconcile.Request {
			u, ok := obj.(*networkingv1alpha.Domain)
			if !ok {
				return nil
			}
			// Find upstream DNSZone(s) in the same namespace as the Domain.
			var zones dnsv1alpha1.DNSZoneList
			if err := cl.GetClient().List(ctx, &zones,
				client.InNamespace(obj.GetNamespace()),
				client.MatchingFieldsSelector{Selector: fields.OneTermEqualSelector("spec.domainName", u.Spec.DomainName)},
			); err != nil {
				return nil
			}
			// TODO: ideally there is only ever one DNSZone with the same spec.domainName in the same namespace.
			var reqs []mcreconcile.Request
			for i := range zones.Items {
				reqs = append(reqs, mcreconcile.Request{
					ClusterName: clusterName,
					Request:     crreconcile.Request{NamespacedName: types.NamespacedName{Namespace: zones.Items[i].Namespace, Name: zones.Items[i].Name}},
				})
			}
			return reqs
		})
	})

	// Downstream watch (wake upstream on changes)
	src := mcsource.TypedKind(
		&dnsv1alpha1.DNSZone{},
		downstreamclient.TypedEnqueueRequestForUpstreamOwner[*dnsv1alpha1.DNSZone](&dnsv1alpha1.DNSZone{}),
	)
	clusterSrc, err := src.ForCluster("", downstreamCl)
	if err != nil {
		return fmt.Errorf("failed to build downstream watch for %s: %w", dnsv1alpha1.GroupVersion.WithKind("DNSZone").String(), err)
	}
	b = b.WatchesRawSource(clusterSrc)

	return b.Named("dnszone-replicator").Complete(r)
}
