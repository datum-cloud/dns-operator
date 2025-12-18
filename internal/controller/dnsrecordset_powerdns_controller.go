// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	pdnsclient "go.miloapis.com/dns-operator/internal/pdns"
)

// PowerDNSRecordSetReconcileRequest scopes reconciliation to a single (zone, type, owner name) tuple.
type PowerDNSRecordSetReconcileRequest struct {
	ctrl.Request
	RecordSetType string
	RecordSetName string
}

// DNSRecordSetPowerDNSReconciler aggregates DNSRecordSets targeting the same
// zone/type/name tuple and ensures PDNS convergence plus per-record status updates.
type DNSRecordSetPowerDNSReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	PDNS   pdnsclient.Interface
}

// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones,verbs=get;list;watch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszoneclasses,verbs=get;list;watch

func (r *DNSRecordSetPowerDNSReconciler) Reconcile(
	ctx context.Context,
	req PowerDNSRecordSetReconcileRequest,
) (reconcile.Result, error) {
	logger := logf.FromContext(ctx).WithValues(
		"zoneNamespace", req.Namespace,
		"zoneName", req.Name,
		"recordType", req.RecordSetType,
		"recordName", req.RecordSetName,
	)
	logger.Info("powerdns reconcile start")

	if req.RecordSetType == "" || req.RecordSetName == "" {
		logger.Info("request missing type or name; skipping")
		return reconcile.Result{}, nil
	}

	var zone dnsv1alpha1.DNSZone
	if err := r.Get(ctx, req.NamespacedName, &zone); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("zone not found; skipping")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	if !zone.DeletionTimestamp.IsZero() {
		logger.Info("zone deleting; skipping")
		return reconcile.Result{}, nil
	}

	var zc dnsv1alpha1.DNSZoneClass
	if err := r.Get(ctx, client.ObjectKey{Name: zone.Spec.DNSZoneClassName}, &zc); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("zone class not found; skipping")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	if zc.Spec.ControllerName != ControllerNamePowerDNS {
		logger.Info("zone controller not powerdns; skipping")
		return reconcile.Result{}, nil
	}

	var rsList dnsv1alpha1.DNSRecordSetList
	if err := r.List(
		ctx,
		&rsList,
		client.InNamespace(req.Namespace),
		client.MatchingFields{
			"spec.DNSZoneRef.Name": zone.Name,
			"spec.recordType":      req.RecordSetType,
		},
	); err != nil {
		return reconcile.Result{}, err
	}

	owningRecordSets, staleStatusRecordSets := splitRecordSetsByName(&rsList, req.RecordSetType, req.RecordSetName)

	var owner *dnsv1alpha1.DNSRecordSet
	if len(owningRecordSets) > 0 {
		sort.SliceStable(owningRecordSets, func(i, j int) bool {
			if owningRecordSets[i].CreationTimestamp.Equal(&owningRecordSets[j].CreationTimestamp) {
				return owningRecordSets[i].Name < owningRecordSets[j].Name
			}
			return owningRecordSets[i].CreationTimestamp.Before(&owningRecordSets[j].CreationTimestamp)
		})
		owner = owningRecordSets[0]
	}

	var pdnsErr error
	if owner == nil {
		pdnsErr = r.PDNS.DeleteRRSet(ctx, zone.Spec.DomainName, req.RecordSetType, req.RecordSetName)
	} else {
		entries := filterRecordEntries(owner, req.RecordSetName)
		payload, ok := pdnsclient.BuildOwnerRRSet(zone.Spec.DomainName, dnsv1alpha1.RRType(req.RecordSetType), req.RecordSetName, entries)
		if !ok || len(payload.Records) == 0 {
			pdnsErr = r.PDNS.DeleteRRSet(ctx, zone.Spec.DomainName, req.RecordSetType, req.RecordSetName)
		} else {
			pdnsErr = r.PDNS.ReplaceRRSet(ctx, zone.Spec.DomainName, req.RecordSetType, req.RecordSetName, payload.TTL, payload.Records)
		}
	}
	if pdnsErr != nil {
		logger.Error(pdnsErr, "pdns apply failed")
	}

	statusErr := r.updateStatuses(ctx, req.RecordSetName, owningRecordSets, staleStatusRecordSets, owner, pdnsErr)
	if statusErr != nil {
		return reconcile.Result{}, statusErr
	}

	if pdnsErr != nil {
		return reconcile.Result{}, pdnsErr
	}

	logger.Info("powerdns reconcile complete")
	return reconcile.Result{}, nil
}

func (r *DNSRecordSetPowerDNSReconciler) updateStatuses(
	ctx context.Context,
	recordName string,
	owningRecordSets []*dnsv1alpha1.DNSRecordSet,
	staleStatusRecordSets []*dnsv1alpha1.DNSRecordSet,
	owner *dnsv1alpha1.DNSRecordSet,
	pdnsErr error,
) error {
	for _, rs := range owningRecordSets {
		isOwner := owner != nil && rs.Name == owner.Name && rs.UID == owner.UID
		if err := r.setRecordProgrammedCondition(ctx, rs, recordName, isOwner, pdnsErr); err != nil {
			return err
		}
	}
	for _, rs := range staleStatusRecordSets {
		if err := r.removeRecordStatus(ctx, rs, recordName); err != nil {
			return err
		}
	}
	return nil
}

func (r *DNSRecordSetPowerDNSReconciler) setRecordProgrammedCondition(
	ctx context.Context,
	rs *dnsv1alpha1.DNSRecordSet,
	name string,
	isOwner bool,
	pdnsErr error,
) error {
	base := rs.DeepCopy()
	st := ensureRecordStatus(&rs.Status, name)

	newCond := metav1.Condition{
		Type:               CondProgrammed,
		ObservedGeneration: rs.Generation,
	}

	switch {
	case !recordNameInSpec(rs, name):
		newCond.Status = metav1.ConditionFalse
		newCond.Reason = ReasonPending
		newCond.Message = "Record no longer present in spec"
	case !isOwner:
		newCond.Status = metav1.ConditionFalse
		newCond.Reason = ReasonNotOwner
		newCond.Message = "Another DNSRecordSet owns this record"
	case pdnsErr != nil:
		newCond.Status = metav1.ConditionFalse
		newCond.Reason = ReasonPDNSError
		newCond.Message = pdnsErr.Error()
	default:
		newCond.Status = metav1.ConditionTrue
		newCond.Reason = ReasonProgrammed
		newCond.Message = "Record successfully applied to PDNS"
	}

	// preserve LastTransitionTime when status didn't change
	if existing := apimeta.FindStatusCondition(st.Conditions, CondProgrammed); existing != nil &&
		existing.Status == newCond.Status {
		newCond.LastTransitionTime = existing.LastTransitionTime
	} else {
		newCond.LastTransitionTime = metav1.Now()
	}

	apimeta.SetStatusCondition(&st.Conditions, newCond)
	pruneStaleRecordStatuses(&rs.Status, rs.Spec.Records)
	refreshProgrammedCondition(rs)

	if equality.Semantic.DeepEqual(base.Status, rs.Status) {
		return nil
	}
	sortRecordStatuses(&rs.Status)
	return r.Status().Patch(ctx, rs, client.MergeFrom(base))
}

func (r *DNSRecordSetPowerDNSReconciler) removeRecordStatus(
	ctx context.Context,
	rs *dnsv1alpha1.DNSRecordSet,
	name string,
) error {
	idx, ok := recordStatusIndex(&rs.Status, name)
	if !ok {
		return nil
	}
	base := rs.DeepCopy()
	rs.Status.RecordSets = append(rs.Status.RecordSets[:idx], rs.Status.RecordSets[idx+1:]...)
	refreshProgrammedCondition(rs)
	sortRecordStatuses(&rs.Status)
	return r.Status().Patch(ctx, rs, client.MergeFrom(base))
}

func ensureRecordStatus(status *dnsv1alpha1.DNSRecordSetStatus, name string) *dnsv1alpha1.RecordSetStatus {
	for i := range status.RecordSets {
		if status.RecordSets[i].Name == name {
			return &status.RecordSets[i]
		}
	}
	status.RecordSets = append(status.RecordSets, dnsv1alpha1.RecordSetStatus{Name: name})
	return &status.RecordSets[len(status.RecordSets)-1]
}

func recordStatusIndex(status *dnsv1alpha1.DNSRecordSetStatus, name string) (int, bool) {
	for i := range status.RecordSets {
		if status.RecordSets[i].Name == name {
			return i, true
		}
	}
	return -1, false
}

func sortRecordStatuses(status *dnsv1alpha1.DNSRecordSetStatus) {
	sort.SliceStable(status.RecordSets, func(i, j int) bool {
		return status.RecordSets[i].Name < status.RecordSets[j].Name
	})
}

func pruneStaleRecordStatuses(status *dnsv1alpha1.DNSRecordSetStatus, records []dnsv1alpha1.RecordEntry) {
	active := sets.NewString()
	for _, rec := range records {
		active.Insert(rec.Name)
	}
	filtered := status.RecordSets[:0]
	for _, st := range status.RecordSets {
		if active.Has(st.Name) {
			filtered = append(filtered, st)
		}
	}
	status.RecordSets = filtered
}

func refreshProgrammedCondition(rs *dnsv1alpha1.DNSRecordSet) {
	names := sets.NewString()
	for _, rec := range rs.Spec.Records {
		names.Insert(rec.Name)
	}
	if names.Len() == 0 {
		apimeta.RemoveStatusCondition(&rs.Status.Conditions, CondProgrammed)
		return
	}

	newCond := metav1.Condition{
		Type:               CondProgrammed,
		ObservedGeneration: rs.Generation,
	}

	reason := ReasonProgrammed
	message := "All records programmed"
	allTrue := true

	for name := range names {
		status := getRecordStatus(rs, name)
		recordCond := apimeta.FindStatusCondition(status.Conditions, CondProgrammed)
		if recordCond == nil || recordCond.Status != metav1.ConditionTrue {
			allTrue = false
			if recordCond == nil {
				reason = ReasonPending
				message = fmt.Sprintf("Record %q pending", name)
			} else {
				reason = recordCond.Reason
				message = fmt.Sprintf("Record %q: %s", name, recordCond.Message)
			}
			break
		}
	}

	if allTrue {
		newCond.Status = metav1.ConditionTrue
		newCond.Reason = ReasonProgrammed
		newCond.Message = message
	} else {
		newCond.Status = metav1.ConditionFalse
		newCond.Reason = reason
		newCond.Message = message
	}

	if existing := apimeta.FindStatusCondition(rs.Status.Conditions, CondProgrammed); existing != nil &&
		existing.Status == newCond.Status {
		newCond.LastTransitionTime = existing.LastTransitionTime
	} else {
		newCond.LastTransitionTime = metav1.Now()
	}

	apimeta.SetStatusCondition(&rs.Status.Conditions, newCond)
}

func getRecordStatus(rs *dnsv1alpha1.DNSRecordSet, name string) dnsv1alpha1.RecordSetStatus {
	for _, st := range rs.Status.RecordSets {
		if st.Name == name {
			return st
		}
	}
	return dnsv1alpha1.RecordSetStatus{Name: name}
}

func recordNameInSpec(rs *dnsv1alpha1.DNSRecordSet, name string) bool {
	for _, rec := range rs.Spec.Records {
		if rec.Name == name {
			return true
		}
	}
	return false
}

func recordStatusExists(rs *dnsv1alpha1.DNSRecordSet, name string) bool {
	_, ok := recordStatusIndex(&rs.Status, name)
	return ok
}

func filterRecordEntries(rs *dnsv1alpha1.DNSRecordSet, name string) []dnsv1alpha1.RecordEntry {
	filtered := make([]dnsv1alpha1.RecordEntry, 0, len(rs.Spec.Records))
	for _, rec := range rs.Spec.Records {
		if rec.Name == name {
			filtered = append(filtered, rec)
		}
	}
	return filtered
}

func splitRecordSetsByName(
	list *dnsv1alpha1.DNSRecordSetList,
	recordType, recordName string,
) (owningRecordSets, staleStatusRecordSets []*dnsv1alpha1.DNSRecordSet) {
	for i := range list.Items {
		rs := &list.Items[i]
		if string(rs.Spec.RecordType) != recordType {
			continue
		}
		if recordNameInSpec(rs, recordName) {
			owningRecordSets = append(owningRecordSets, rs)
		} else if recordStatusExists(rs, recordName) {
			staleStatusRecordSets = append(staleStatusRecordSets, rs)
		}
	}
	return
}

func (r *DNSRecordSetPowerDNSReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.PDNS == nil {
		cli, err := pdnsclient.NewFromEnv()
		if err != nil {
			return err
		}
		r.PDNS = cli
	}

	ctx := context.Background()
	if err := mgr.GetFieldIndexer().IndexField(ctx,
		&dnsv1alpha1.DNSRecordSet{}, "spec.recordType",
		func(obj client.Object) []string {
			rs := obj.(*dnsv1alpha1.DNSRecordSet)
			if rs.Spec.RecordType == "" {
				return nil
			}
			return []string{string(rs.Spec.RecordType)}
		}); err != nil {
		return err
	}

	rl := workqueue.NewTypedItemExponentialFailureRateLimiter[PowerDNSRecordSetReconcileRequest](1*time.Second, 30*time.Second)
	c, err := controller.NewTyped("dnsrecordset-powerdns", mgr, controller.TypedOptions[PowerDNSRecordSetReconcileRequest]{
		Reconciler:              r,
		RateLimiter:             rl,
		MaxConcurrentReconciles: 4,
	})
	if err != nil {
		return err
	}

	rsHandler := handler.TypedFuncs[*dnsv1alpha1.DNSRecordSet, PowerDNSRecordSetReconcileRequest]{
		CreateFunc: func(ctx context.Context, evt event.TypedCreateEvent[*dnsv1alpha1.DNSRecordSet], q workqueue.TypedRateLimitingInterface[PowerDNSRecordSetReconcileRequest]) {
			enqueueRecordSetRequests(evt.Object, q)
		},
		UpdateFunc: func(ctx context.Context, evt event.TypedUpdateEvent[*dnsv1alpha1.DNSRecordSet], q workqueue.TypedRateLimitingInterface[PowerDNSRecordSetReconcileRequest]) {
			enqueueRecordSetRequests(evt.ObjectNew, q)
			enqueueRecordSetRequests(evt.ObjectOld, q)
		},
		DeleteFunc: func(ctx context.Context, evt event.TypedDeleteEvent[*dnsv1alpha1.DNSRecordSet], q workqueue.TypedRateLimitingInterface[PowerDNSRecordSetReconcileRequest]) {
			enqueueRecordSetRequests(evt.Object, q)
		},
	}
	if err := c.Watch(
		source.TypedKind(mgr.GetCache(), &dnsv1alpha1.DNSRecordSet{}, rsHandler),
	); err != nil {
		return err
	}

	zoneHandler := handler.TypedFuncs[*dnsv1alpha1.DNSZone, PowerDNSRecordSetReconcileRequest]{
		CreateFunc: func(ctx context.Context, evt event.TypedCreateEvent[*dnsv1alpha1.DNSZone], q workqueue.TypedRateLimitingInterface[PowerDNSRecordSetReconcileRequest]) {
			r.enqueueZoneRecords(ctx, evt.Object, q)
		},
		UpdateFunc: func(ctx context.Context, evt event.TypedUpdateEvent[*dnsv1alpha1.DNSZone], q workqueue.TypedRateLimitingInterface[PowerDNSRecordSetReconcileRequest]) {
			r.enqueueZoneRecords(ctx, evt.ObjectNew, q)
		},
	}
	return c.Watch(
		source.TypedKind(mgr.GetCache(), &dnsv1alpha1.DNSZone{}, zoneHandler),
	)
}

func enqueueRecordSetRequests(rs *dnsv1alpha1.DNSRecordSet, q workqueue.TypedRateLimitingInterface[PowerDNSRecordSetReconcileRequest]) {
	if rs == nil || rs.Spec.DNSZoneRef.Name == "" {
		return
	}
	seen := sets.NewString()
	for _, rec := range rs.Spec.Records {
		if rec.Name == "" || seen.Has(rec.Name) {
			continue
		}
		seen.Insert(rec.Name)
		q.Add(PowerDNSRecordSetReconcileRequest{
			Request: ctrl.Request{
				NamespacedName: client.ObjectKey{
					Namespace: rs.Namespace,
					Name:      rs.Spec.DNSZoneRef.Name,
				},
			},
			RecordSetType: string(rs.Spec.RecordType),
			RecordSetName: rec.Name,
		})
	}
}

func (r *DNSRecordSetPowerDNSReconciler) enqueueZoneRecords(
	ctx context.Context,
	zone *dnsv1alpha1.DNSZone,
	q workqueue.TypedRateLimitingInterface[PowerDNSRecordSetReconcileRequest],
) {
	if zone == nil {
		return
	}
	var rsList dnsv1alpha1.DNSRecordSetList
	if err := r.List(ctx, &rsList,
		client.InNamespace(zone.Namespace),
		client.MatchingFields{"spec.DNSZoneRef.Name": zone.Name},
	); err != nil {
		logf.FromContext(ctx).Error(err, "failed to list recordsets for zone", "zone", zone.Name)
		return
	}
	for i := range rsList.Items {
		enqueueRecordSetRequests(&rsList.Items[i], q)
	}
}
