package admission

import (
	"context"
	"encoding/json"
	"fmt"

	dns "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	admissionv1 "k8s.io/api/admission/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	admission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func registrationScopes(ctx context.Context, cl client.Client, zone *dns.DNSZone) (model.ReservedScopes, error) {
	var regs dns.DNSRegistrationList
	if err := cl.List(ctx, &regs, client.InNamespace(zone.Namespace)); err != nil {
		return nil, err
	}
	return model.RegistrationScopes(zone.Name, zone.Spec.DomainName, regs.Items), nil
}

// Check both old and new intent so stripping descendants, moving a declaration,
// deleting it, and removing its finalizers cannot bypass reservation ownership.
func authorizeReservationMutation(ctx context.Context, cl client.Client, req admission.Request) error {
	var current dns.DNSRegistration
	if err := decode(req, &current); err != nil {
		return err
	}
	declarations := []dns.DNSRegistration{current}
	if req.Operation == admissionv1.Update && len(req.OldObject.Raw) > 0 {
		var old dns.DNSRegistration
		if err := json.Unmarshal(req.OldObject.Raw, &old); err != nil {
			return err
		}
		declarations = append(declarations, old)
	}
	for i, reg := range declarations {
		if len(reg.Spec.ReservedDescendants) == 0 {
			continue
		}
		var zone dns.DNSZone
		err := cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: reg.Spec.DNSZoneRef.Name}, &zone)
		if apierrors.IsNotFound(err) && (req.Operation == admissionv1.Delete || !current.DeletionTimestamp.IsZero()) {
			// A missing managed zone must not release its resolver's reservations.
			// With no matching resolver, ordinary orphaned tenant cleanup is safe.
			var resolvers dns.DNSResolverContextList
			if err := cl.List(ctx, &resolvers, client.InNamespace(req.Namespace)); err != nil {
				return err
			}
			for _, resolver := range resolvers.Items {
				if reg.Spec.DNSZoneRef.Name == model.ManagedZoneName(string(resolver.UID)) {
					if err := authorizeRegistration(ctx, cl, req, reg.Name, "reserve"); err != nil {
						return err
					}
					break
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		if zone.Spec.Visibility != dns.DNSZoneVisibilityPrivate || !model.IsManagedZone(&zone) {
			continue
		}
		if err := authorizeRegistration(ctx, cl, req, reg.Name, "reserve"); err != nil {
			return err
		}
		if i == 0 && req.Operation != admissionv1.Delete && current.DeletionTimestamp.IsZero() {
			if err := model.ValidateReservation(&reg, zone.Spec.DomainName); err != nil {
				return err
			}
		}
	}
	return nil
}

func authorizeRegistration(ctx context.Context, cl client.Client, req admission.Request, name, verb string) error {
	if name == "" {
		return fmt.Errorf("reservation requires a named registration")
	}
	extra := map[string]authorizationv1.ExtraValue{}
	for k, v := range req.UserInfo.Extra {
		extra[k] = authorizationv1.ExtraValue(v)
	}
	review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: req.UserInfo.Username, UID: req.UserInfo.UID, Groups: req.UserInfo.Groups, Extra: extra,
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: req.Namespace, Group: dns.GroupVersion.Group, Resource: registrationResource, Name: name, Verb: verb},
	}}
	if err := cl.Create(ctx, review); err != nil {
		return fmt.Errorf("reservation authorization unavailable: %w", err)
	}
	if !review.Status.Allowed {
		return fmt.Errorf("caller is not authorized to %s registration %q", verb, name)
	}
	return nil
}
