package controlplane

import (
	"context"

	dns "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type reservationVersion struct {
	Generation int64
	Deleting   bool
}

func reservationVersions(regs []dns.DNSRegistration, zone string) map[string]reservationVersion {
	out := map[string]reservationVersion{}
	for _, reg := range regs {
		if len(reg.Spec.ReservedDescendants) > 0 && (zone == "" || reg.Spec.DNSZoneRef.Name == zone) {
			out[reg.Spec.DNSZoneRef.Name+"/"+string(reg.UID)] = reservationVersion{reg.Generation, !reg.DeletionTimestamp.IsZero()}
		}
	}
	return out
}

// A zone worker must not publish using an association accepted before a service
// created its reservation. The project pass separately persists the rejection.
func (r *Reconciler) reservationSafeAssociations(ctx context.Context, z *dns.DNSZone, assocs []dns.DNSZoneAssociation) ([]dns.DNSZoneAssociation, error) {
	var regs dns.DNSRegistrationList
	loaded := false
	out := append([]dns.DNSZoneAssociation(nil), assocs...)
	for i, a := range out {
		if a.Spec.DNSZoneRef.Name != z.Name {
			continue
		}
		var resolver dns.DNSResolverContext
		err := r.sourceReader().Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: a.Spec.ResolverContextRef.Name}, &resolver)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		if err == nil && resolver.Spec.ManagedNamespace.Enabled && (z.Name != model.ManagedZoneName(string(resolver.UID)) || !model.IsManagedZone(z)) && !loaded {
			if err := r.sourceReader().List(ctx, &regs, client.InNamespace(z.Namespace)); err != nil {
				return nil, err
			}
			loaded = true
		}
		if err == nil && model.ShadowsContext(z, &resolver, regs.Items) {
			out[i] = *a.DeepCopy()
			apimeta.SetStatusCondition(&out[i].Status.Conditions, metav1.Condition{Type: dnsValueAccepted, Status: metav1.ConditionFalse, Reason: dnsValueReservedScope, Message: "zone shadows a service reservation", ObservedGeneration: a.Generation})
		}
	}
	return out, nil
}
