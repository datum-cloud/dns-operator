// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dns "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const inputZoneIndex = "private-dns.input-zone"
const inputRegistrationIndex = "private-dns.input-registration"

var cachedPlanMismatches = prometheus.NewCounter(prometheus.CounterOpts{Name: "internal_dns_cached_plan_mismatches_total", Help: "Cached zone plans differing from a live resync compile."})

func init() { metrics.Registry.MustRegister(cachedPlanMismatches) }

type zoneInputs struct {
	registrations []dns.DNSRegistration
	contributions []dns.DNSRecordContribution
	records       []dns.DNSRecordSet
}
type zoneInputState struct {
	liveCompiled bool
	epoch        int64
	baseline     zoneInputs
	ready        bool
	shadowAt     time.Time
	accepted     map[types.UID]bool
}
type zoneInputSession struct {
	state  *zoneInputState
	cached bool
	guard  func(CompileInput) error
}
type zoneInputContextKey struct{}

func inputSession(ctx context.Context) *zoneInputSession {
	s, _ := ctx.Value(zoneInputContextKey{}).(*zoneInputSession)
	return s
}

func (r *Reconciler) setupInputIndexes(mgr ctrl.Manager) error {
	for _, obj := range []client.Object{&dns.DNSRegistration{}, &dns.DNSRecordSet{}} {
		if err := mgr.GetFieldIndexer().IndexField(context.Background(), obj, inputZoneIndex, func(o client.Object) []string {
			switch v := o.(type) {
			case *dns.DNSRegistration:
				return []string{v.Spec.DNSZoneRef.Name}
			case *dns.DNSRecordSet:
				return []string{v.Spec.DNSZoneRef.Name}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &dns.DNSRecordContribution{}, inputRegistrationIndex, func(o client.Object) []string {
		return []string{o.(*dns.DNSRecordContribution).Spec.RegistrationRef.Name}
	}); err != nil {
		return err
	}
	if r.InputReader == nil {
		r.InputReader = mgr.GetCache()
	}
	return nil
}

func (r *Reconciler) readZoneInputs(ctx context.Context, z *dns.DNSZone, cached bool) (zoneInputs, error) {
	reader := r.sourceReader()
	options := []client.ListOption{client.InNamespace(z.Namespace)}
	if cached {
		reader = r.InputReader
		options = append(options, client.MatchingFields{inputZoneIndex: z.Name})
	}
	var regs dns.DNSRegistrationList
	var records dns.DNSRecordSetList
	registrationReader, registrationOptions := reader, options
	// Reservations are authority, never trust an informer that may not yet know
	// a newly created policy. Keep record/contribution payload caching unchanged.
	if model.IsManagedZone(z) {
		registrationReader = r.sourceReader()
		registrationOptions = []client.ListOption{client.InNamespace(z.Namespace)}
	}
	if err := registrationReader.List(ctx, &regs, registrationOptions...); err != nil {
		return zoneInputs{}, err
	}
	if err := reader.List(ctx, &records, options...); err != nil {
		return zoneInputs{}, err
	}
	out := zoneInputs{}
	names := map[string]bool{}
	for _, reg := range regs.Items {
		if reg.Spec.DNSZoneRef.Name == z.Name {
			out.registrations = append(out.registrations, reg)
			names[reg.Name] = true
		}
	}
	for _, rec := range records.Items {
		if rec.Spec.DNSZoneRef.Name == z.Name {
			out.records = append(out.records, rec)
		}
	}
	if cached {
		for name := range names {
			var cs dns.DNSRecordContributionList
			if err := reader.List(ctx, &cs, client.InNamespace(z.Namespace), client.MatchingFields{inputRegistrationIndex: name}); err != nil {
				return zoneInputs{}, err
			}
			out.contributions = append(out.contributions, cs.Items...)
		}
	} else {
		var cs dns.DNSRecordContributionList
		if err := reader.List(ctx, &cs, client.InNamespace(z.Namespace)); err != nil {
			return zoneInputs{}, err
		}
		for _, c := range cs.Items {
			if names[c.Spec.RegistrationRef.Name] {
				out.contributions = append(out.contributions, c)
			}
		}
	}
	return out, nil
}

func (in zoneInputs) objects() map[string]client.Object {
	out := map[string]client.Object{}
	for i := range in.registrations {
		o := &in.registrations[i]
		out["registration/"+o.Name] = o
	}
	for i := range in.contributions {
		o := &in.contributions[i]
		out["contribution/"+o.Name] = o
	}
	for i := range in.records {
		o := &in.records[i]
		out["record/"+o.Name] = o
	}
	return out
}

func inputsCaughtUp(cached, live zoneInputs) bool {
	c, l := cached.objects(), live.objects()
	if len(c) != len(l) {
		return false
	}
	for name, want := range l {
		got := c[name]
		if got == nil || got.GetUID() != want.GetUID() || got.GetGeneration() < want.GetGeneration() || !reflect.DeepEqual(got.GetDeletionTimestamp(), want.GetDeletionTimestamp()) {
			return false
		}
	}
	return true
}

func (r *Reconciler) selectZoneInputs(ctx context.Context, z *dns.DNSZone, own *dns.DNSPublicationOwnership) (zoneInputs, *zoneInputSession, error) {
	if !r.Options.CachedZoneInputs || r.InputReader == nil {
		in, err := r.readZoneInputs(ctx, z, false)
		return in, nil, err
	}
	value, _ := r.zoneInputs.LoadOrStore(z.UID, &zoneInputState{})
	state := value.(*zoneInputState)
	session := &zoneInputSession{state: state}
	cached, err := r.readZoneInputs(ctx, z, true)
	if err != nil {
		return zoneInputs{}, nil, err
	}
	if state.epoch != own.Spec.WriterEpoch {
		*state = zoneInputState{epoch: own.Spec.WriterEpoch}
	} else if state.liveCompiled && (state.ready || inputsCaughtUp(cached, state.baseline)) {
		state.ready = true
		session.cached = true
		return cached, session, nil
	}
	live, err := r.readZoneInputs(ctx, z, false)
	if err == nil {
		state.baseline = live
	}
	return live, session, err
}

func cacheConflict(name string) error {
	return apierrors.NewConflict(schema.GroupResource{Group: dns.GroupVersion.Group, Resource: "dnszones"}, name, fmt.Errorf("zone inputs changed during publication"))
}

func (r *Reconciler) pinZoneRegistrations(ctx context.Context, z *dns.DNSZone, regs []dns.DNSRegistration) (map[types.UID]dns.DNSObjectReference, error) {
	pins := map[types.UID]dns.DNSObjectReference{}
	for i := range regs {
		reg := &regs[i]
		ref := dns.DNSObjectReference{Name: z.Name}
		if z.DeletionTimestamp.IsZero() {
			ref.UID, ref.Generation = z.UID, z.Generation
		}
		pinned, _, err := r.pinReferences(ctx, reg, map[string]dns.DNSObjectReference{"zone": ref})
		if err != nil {
			return nil, err
		}
		pins[reg.UID] = pinned["zone"]
	}
	return pins, nil
}

func (r *Reconciler) cachedFencesCurrent(ctx context.Context, z *dns.DNSZone, in *CompileInput) (bool, error) {
	if in.Previous == nil {
		return true, nil
	}
	regs := map[types.UID]int64{}
	for _, reg := range in.Registrations {
		regs[reg.UID] = reg.Generation
	}
	for _, fence := range in.Previous.Registrations {
		if generation, ok := regs[fence.UID]; !ok || generation < fence.Generation {
			return false, nil
		}
	}
	cs := map[types.UID]int64{}
	for _, c := range in.Contributions {
		cs[c.UID] = c.Generation
	}
	for _, fence := range in.Previous.Contributions {
		if fence.Retired {
			continue
		}
		if generation, ok := cs[fence.UID]; ok {
			if generation < fence.Generation {
				return false, nil
			}
			continue
		}
		if fence.SourceName == "" {
			return false, nil
		}
		var c dns.DNSRecordContribution
		err := r.sourceReader().Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: fence.SourceName}, &c)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if c.UID == fence.UID && c.Generation < fence.Generation {
			return false, cacheConflict(z.Name)
		}
		in.Contributions = slices.DeleteFunc(in.Contributions, func(old dns.DNSRecordContribution) bool { return old.Name == c.Name })
		in.Contributions = append(in.Contributions, c)
	}
	return true, nil
}

func losesAccepted(result CompileResult, accepted map[types.UID]bool) bool {
	for uid, was := range accepted {
		if was && !result.Accepted[uid] {
			return true
		}
	}
	return false
}

func (r *Reconciler) compileCachedInputs(ctx context.Context, in CompileInput) (CompileResult, CompileInput, error) {
	session := inputSession(ctx)
	if session == nil {
		result, err := Compile(in)
		return result, in, err
	}
	live := func() (CompileResult, CompileInput, error) {
		inputs, err := r.readZoneInputs(ctx, in.Zone, false)
		if err != nil {
			return CompileResult{}, in, err
		}
		session.state.baseline, session.state.ready = inputs, false
		in.Registrations, in.Contributions, in.StaticRecords = inputs.registrations, inputs.contributions, inputs.records
		in.RegistrationZoneReferences, err = r.pinZoneRegistrations(ctx, in.Zone, in.Registrations)
		if err != nil {
			return CompileResult{}, in, err
		}
		in.Contributions, err = r.confirmContributions(ctx, in.Zone.Namespace, in.Contributions, in.Previous)
		if err != nil {
			return CompileResult{}, in, err
		}
		result, err := Compile(in)
		return result, in, err
	}
	if session.cached {
		current, err := r.cachedFencesCurrent(ctx, in.Zone, &in)
		if err != nil {
			return CompileResult{}, in, err
		}
		if !current {
			return live()
		}
	}
	result, err := Compile(in)
	if err != nil {
		return result, in, err
	}
	if session.cached && losesAccepted(result, session.state.accepted) {
		return live()
	}
	if session.cached && in.Now.Sub(session.state.shadowAt) >= zoneResync {
		checked, liveInput, err := live()
		if err != nil {
			return CompileResult{}, in, err
		}
		session.state.shadowAt = in.Now
		if !reflect.DeepEqual(result.Plan, checked.Plan) {
			cachedPlanMismatches.Inc()
		}
		return checked, liveInput, nil
	}
	return result, in, nil
}

type accessSnapshot struct {
	reservations map[string]reservationVersion
	zone         dns.DNSZone
	associations []dns.DNSZoneAssociation
	contexts     []dns.DNSResolverContext
	pins         map[types.UID]associationReferences
	bindings     []dns.DNSResolverBinding
}

func (r *Reconciler) zoneAccessSnapshot(ctx context.Context, z *dns.DNSZone) (accessSnapshot, error) {
	out := accessSnapshot{associations: []dns.DNSZoneAssociation{}}

	if err := r.sourceReader().Get(ctx, client.ObjectKeyFromObject(z), &out.zone); err != nil {
		return out, err
	}
	var assocs dns.DNSZoneAssociationList
	if err := r.sourceReader().List(ctx, &assocs, client.InNamespace(z.Namespace)); err != nil {
		return out, err
	}
	for _, a := range assocs.Items {
		if a.Spec.DNSZoneRef.Name != z.Name {
			continue
		}
		out.associations = append(out.associations, a)
		var c dns.DNSResolverContext
		err := r.sourceReader().Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: a.Spec.ResolverContextRef.Name}, &c)
		if err != nil && !apierrors.IsNotFound(err) {
			return out, err
		}
		out.contexts = append(out.contexts, c)
	}
	needReservations := model.IsManagedZone(z)
	for _, c := range out.contexts {
		needReservations = needReservations || c.Spec.ManagedNamespace.Enabled
	}
	if needReservations {
		var regs dns.DNSRegistrationList
		if err := r.sourceReader().List(ctx, &regs, client.InNamespace(z.Namespace)); err != nil {
			return out, err
		}
		out.reservations = reservationVersions(regs.Items, "")
	}
	var err error
	out.pins, err = r.associationReferences(ctx, out.associations)
	if err != nil {
		return out, err
	}
	var bindings dns.DNSResolverBindingList
	if err := r.listProjectBindings(ctx, &bindings); err != nil {
		return out, err
	}
	out.bindings = bindings.Items
	return out, nil
}

func (r *Reconciler) guardZoneAccess(ctx context.Context, z *dns.DNSZone, assocs []dns.DNSZoneAssociation) (func(CompileInput) error, error) {
	before, err := r.zoneAccessSnapshot(ctx, z)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(before.zone.Spec, z.Spec) || before.zone.UID != z.UID || before.zone.Generation != z.Generation || !reflect.DeepEqual(before.zone.DeletionTimestamp, z.DeletionTimestamp) || !reflect.DeepEqual(before.associations, assocs) {
		return nil, cacheConflict(z.Name)
	}
	for i, a := range before.associations {
		if !associationAccepted(&a) || !a.DeletionTimestamp.IsZero() || !z.DeletionTimestamp.IsZero() {
			continue
		}
		c := before.contexts[i]
		pin := before.pins[a.UID].Context
		ready := apimeta.FindStatusCondition(c.Status.Conditions, "Ready")
		if c.UID != pin.UID || c.UID == "" || !c.DeletionTimestamp.IsZero() || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != c.Generation {
			return nil, cacheConflict(z.Name)
		}
	}
	return func(in CompileInput) error {
		if model.IsManagedZone(z) {
			expected := map[string]reservationVersion{}
			for k, v := range before.reservations {
				if strings.HasPrefix(k, z.Name+"/") {
					expected[k] = v
				}
			}
			if !reflect.DeepEqual(expected, reservationVersions(in.Registrations, z.Name)) {
				return cacheConflict(z.Name)
			}
		}
		after, err := r.zoneAccessSnapshot(ctx, z)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(before, after) {
			return cacheConflict(z.Name)
		}
		for i := range in.Registrations {
			reg := &in.Registrations[i]
			pins, err := r.readReferences(ctx, reg)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(pins["zone"], in.RegistrationZoneReferences[reg.UID]) {
				return cacheConflict(z.Name)
			}
		}
		return nil
	}, nil
}
