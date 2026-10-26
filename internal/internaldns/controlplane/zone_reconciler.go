// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Queue keys other than projectQueueKey name one zone, or the registration
// whose zone a contribution belongs to.
const (
	zoneQueuePrefix         = "zone/"
	registrationQueuePrefix = "registration/"
)

// zoneQueueDelay coalesces a burst of writes to one zone into one publication.
const zoneQueueDelay = 75 * time.Millisecond

// zoneConflictRetry retries a zone that lost an optimistic lock to the project
// pass or another replica without the exponential backoff an error would get.
const zoneConflictRetry = 200 * time.Millisecond

// staleAssociationRetry waits for the project pass to pin an association
// against the zone's current generation.
const staleAssociationRetry = time.Second

var zoneReconcileDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "internal_dns_zone_reconcile_seconds",
	Help:    "Duration of single-zone reconciles when per-zone reconcile is enabled.",
	Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
})

func init() {
	metrics.Registry.MustRegister(zoneReconcileDuration)
}

func zoneRequest(namespace, zone string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: zoneQueuePrefix + zone}}
}

// zoneEvents queues zone and registration keys after zoneQueueDelay. Other
// kinds keep the project key.
type zoneEvents struct {
	keys func(client.Object) []reconcile.Request
}

func (h zoneEvents) add(q workqueue.TypedRateLimitingInterface[reconcile.Request], objects ...client.Object) {
	seen := map[reconcile.Request]bool{}
	for _, o := range objects {
		if o == nil {
			continue
		}
		for _, req := range h.keys(o) {
			if !seen[req] {
				seen[req] = true
				q.AddAfter(req, zoneQueueDelay)
			}
		}
	}
}

func (h zoneEvents) Create(_ context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.add(q, e.Object)
}

func (h zoneEvents) Update(_ context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.add(q, e.ObjectOld, e.ObjectNew)
}

func (h zoneEvents) Delete(_ context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.add(q, e.Object)
}

func (h zoneEvents) Generic(_ context.Context, e event.GenericEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.add(q, e.Object)
}

var _ handler.EventHandler = zoneEvents{}

func recordSetZone(o client.Object) []reconcile.Request {
	rs, ok := o.(*dnsv1alpha1.DNSRecordSet)
	if !ok || rs.Spec.DNSZoneRef.Name == "" {
		return nil
	}
	return []reconcile.Request{zoneRequest(rs.Namespace, rs.Spec.DNSZoneRef.Name)}
}

func registrationZone(o client.Object) []reconcile.Request {
	reg, ok := o.(*dnsv1alpha1.DNSRegistration)
	if !ok || reg.Spec.DNSZoneRef.Name == "" {
		return nil
	}
	return []reconcile.Request{zoneRequest(reg.Namespace, reg.Spec.DNSZoneRef.Name)}
}

func contributionRegistration(o client.Object) []reconcile.Request {
	c, ok := o.(*dnsv1alpha1.DNSRecordContribution)
	if !ok || c.Spec.RegistrationRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: c.Namespace, Name: registrationQueuePrefix + c.Spec.RegistrationRef.Name}}}
}

func zoneItself(o client.Object) []reconcile.Request {
	return []reconcile.Request{zoneRequest(o.GetNamespace(), o.GetName())}
}

// zoneResync is how often the project pass queues a zone whose project-wide
// inputs have not changed, as a backstop for a missed event.
const zoneResync = 5 * time.Minute

type queuedZone struct {
	inputs string
	at     time.Time
}

// bindingInputs fingerprints the resolver bindings that pick a zone's serving
// targets. Targets read only binding specs and deletion, so status is left out.
func bindingInputs(bindings []dnsv1alpha1.DNSResolverBinding) string {
	h := sha256.New()
	for i := range bindings {
		b := &bindings[i]
		fmt.Fprintf(h, "binding %s %d %v\n", b.UID, b.Generation, b.DeletionTimestamp)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// zoneProjectInputs fingerprints what the project pass decides for a zone: the
// zone itself, the writer epoch it holds, the zone's associations and their
// status, and the resolver bindings. Record inputs are left out; their own
// events queue the zone.
func zoneProjectInputs(z *dnsv1alpha1.DNSZone, own *dnsv1alpha1.DNSPublicationOwnership, assocs []dnsv1alpha1.DNSZoneAssociation, bindings string) string {
	h := sha256.New()
	fmt.Fprintf(h, "zone %s %d %v %v epoch %d bindings %s\n", z.UID, z.Generation, z.DeletionTimestamp, z.Finalizers, own.Spec.WriterEpoch, bindings)
	for i := range assocs {
		a := &assocs[i]
		if a.Spec.DNSZoneRef.Name == z.Name {
			fmt.Fprintf(h, "assoc %s %s\n", a.UID, a.ResourceVersion)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// queueZoneIfChanged queues a zone when its project-wide inputs changed since
// it was last queued, or once per zoneResync.
func (r *Reconciler) queueZoneIfChanged(ctx context.Context, z *dnsv1alpha1.DNSZone, inputs string, now time.Time) {
	if last, ok := r.queuedZones.Load(z.UID); ok {
		q := last.(queuedZone)
		if q.inputs == inputs && now.Sub(q.at) < zoneResync {
			return
		}
	}
	if r.queueZone(ctx, z) {
		r.queuedZones.Store(z.UID, queuedZone{inputs: inputs, at: now})
	}
}

// queueZone hands a zone from the project pass to the zone controller.
func (r *Reconciler) queueZone(ctx context.Context, z *dnsv1alpha1.DNSZone) bool {
	if r.zoneQueue == nil {
		return false
	}
	select {
	case r.zoneQueue <- event.GenericEvent{Object: z}:
		return true
	case <-ctx.Done():
		return false
	}
}

// reconcileQueueKey runs a zone key. A registration key only forwards to its
// zone's key, so one zone never compiles on two workers at once.
func (r *Reconciler) reconcileQueueKey(ctx context.Context, ns, name string) (ctrl.Result, error) {
	if regName, ok := strings.CutPrefix(name, registrationQueuePrefix); ok {
		var reg dnsv1alpha1.DNSRegistration
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: regName}, &reg); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if reg.Spec.DNSZoneRef.Name != "" {
			r.queueZone(ctx, &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: reg.Spec.DNSZoneRef.Name}})
		}
		return ctrl.Result{}, nil
	}
	name = strings.TrimPrefix(name, zoneQueuePrefix)
	if name == "" {
		return ctrl.Result{}, nil
	}
	if slots := r.Options.ZoneSlots; slots != nil {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			return ctrl.Result{}, ctx.Err()
		}
	}
	started := time.Now()
	defer func() { zoneReconcileDuration.Observe(time.Since(started).Seconds()) }()
	result, err := r.reconcileZone(ctx, ns, name)
	if apierrors.IsConflict(err) {
		return ctrl.Result{RequeueAfter: zoneConflictRetry}, nil
	}
	return result, err
}

// reconcileZone compiles and publishes one zone. Project-wide work (contexts,
// associations and their pins, bindings, lease renewal of idle zones) stays
// with the project pass, which queues every zone it touches.
func (r *Reconciler) reconcileZone(ctx context.Context, ns, name string) (ctrl.Result, error) {
	now := r.Options.Now().UTC()
	var z dnsv1alpha1.DNSZone
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &z); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if z.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate {
		return ctrl.Result{}, nil
	}
	if z.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(&z, privateZoneFinalizer) {
		// The project pass adds the finalizer and queues the zone again.
		return ctrl.Result{RequeueAfter: staleAssociationRetry}, nil
	}

	var assocs dnsv1alpha1.DNSZoneAssociationList
	var regs dnsv1alpha1.DNSRegistrationList
	var contributions dnsv1alpha1.DNSRecordContributionList
	var records dnsv1alpha1.DNSRecordSetList
	for _, item := range []client.ObjectList{&assocs, &regs, &contributions, &records} {
		if err := r.List(ctx, item, client.InNamespace(ns)); err != nil {
			return ctrl.Result{}, err
		}
	}
	ctx = withZoneLoopReads(withPinnedAssociationReferences(ctx))

	zoneAssocs := make([]dnsv1alpha1.DNSZoneAssociation, 0, len(assocs.Items))
	for _, a := range assocs.Items {
		if a.Spec.DNSZoneRef.Name == z.Name {
			zoneAssocs = append(zoneAssocs, a)
		}
	}
	pins, err := r.associationReferences(ctx, zoneAssocs)
	if err != nil {
		return ctrl.Result{}, err
	}
	for _, a := range zoneAssocs {
		// An association pinned against an older zone generation would compile
		// as detached and withdraw the zone. Wait for the project pass to pin it.
		if associationAccepted(&a) && a.DeletionTimestamp.IsZero() && z.DeletionTimestamp.IsZero() && !refMatches(pins[a.UID].Zone, z.Name, z.UID, z.Generation) {
			return ctrl.Result{RequeueAfter: staleAssociationRetry}, nil
		}
	}

	registrationZones := map[types.UID]dnsv1alpha1.DNSObjectReference{}
	for i := range regs.Items {
		reg := &regs.Items[i]
		if reg.Spec.DNSZoneRef.Name != z.Name {
			continue
		}
		ref := dnsv1alpha1.DNSObjectReference{Name: z.Name}
		if z.DeletionTimestamp.IsZero() {
			ref.UID, ref.Generation = z.UID, z.Generation
		}
		pinned, _, err := r.pinReferences(ctx, reg, map[string]dnsv1alpha1.DNSObjectReference{"zone": ref})
		if err != nil {
			return ctrl.Result{}, err
		}
		registrationZones[reg.UID] = pinned["zone"]
	}

	own, retryAt, err := r.holdOwnership(ctx, &z)
	if err != nil {
		return ctrl.Result{}, err
	}
	if own == nil {
		return ctrl.Result{RequeueAfter: max(time.Until(retryAt), zoneQueueDelay)}, nil
	}
	released, err := r.compileZone(ctx, &z, own, zoneAssocs, regs.Items, registrationZones, contributions.Items, records.Items, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	pending, err := r.aggregatePublicationStatus(ctx, &z, regs.Items, contributions.Items, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !z.DeletionTimestamp.IsZero() && released {
		base := z.DeepCopy()
		controllerutil.RemoveFinalizer(&z, privateZoneFinalizer)
		if err := r.Patch(ctx, &z, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		r.queuedZones.Delete(z.UID)
		return ctrl.Result{}, nil
	}
	if !z.DeletionTimestamp.IsZero() {
		// The tombstone was just published; check for its acknowledgement.
		return ctrl.Result{RequeueAfter: staleAssociationRetry}, nil
	}
	if pending.IsZero() {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: max(pending.Sub(r.Options.Now().UTC()), zoneQueueDelay)}, nil
}
