// SPDX-License-Identifier: AGPL-3.0-only

// Package admission validates live references between private DNS resources.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	admissionv1 "k8s.io/api/admission/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kubeadmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const subresourceStatus = "status"

const contributionResource = "dnsrecordcontributions"

// Path is where the dns-operator webhook server serves this handler.
const Path = "/validate-internal-dns"

// Resources lists the resources whose writes require live reference checks.
// The ValidatingWebhookConfiguration must match exactly these resources.
var Resources = []string{"dnsrecordsets", "dnszoneassociations", "dnsregistrations", contributionResource, "dnsnamingpolicies", "dnsresolveraccessbindings"}

// Handler uses an uncached client selected by authenticated project context.
type Handler struct {
	Client client.Client

	ReservedScopes model.ReservedScopes

	ClientForRequest func(context.Context, kubeadmission.Request) (client.Client, error)
}

var _ kubeadmission.Handler = (*Handler)(nil)

func (h *Handler) Handle(ctx context.Context, req kubeadmission.Request) kubeadmission.Response {
	if req.Resource.Group != dnsv1alpha1.GroupVersion.Group {
		return kubeadmission.Denied("unexpected API group")
	}
	// The API server authorizes resource and status access through RBAC.
	if req.Operation == admissionv1.Delete {
		return kubeadmission.Allowed("API server authorization applies")
	}
	if req.SubResource == subresourceStatus {
		return kubeadmission.Allowed("API server status authorization applies")
	}
	if !slices.Contains(Resources, req.Resource.Resource) {
		return kubeadmission.Allowed("API server authorization and schema validation apply")
	}
	if req.Operation == admissionv1.Update {
		var object metav1.PartialObjectMetadata
		if err := json.Unmarshal(req.Object.Raw, &object); err != nil {
			return kubeadmission.Denied("invalid resource metadata")
		}
		if !object.DeletionTimestamp.IsZero() {
			return kubeadmission.Allowed("resource finalization does not require live references")
		}
	}
	cl := h.Client
	if h.ClientForRequest != nil {
		var err error
		cl, err = h.ClientForRequest(ctx, req)
		if err != nil {
			return kubeadmission.Errored(http.StatusServiceUnavailable, err)
		}
	}
	if cl == nil {
		return kubeadmission.Errored(http.StatusServiceUnavailable, fmt.Errorf("project API client unavailable"))
	}
	var err error
	switch req.Resource.Resource {
	case "dnsrecordsets":
		err = h.recordSet(ctx, cl, req)
	case "dnsresolveraccessbindings":
		err = h.resolverAccess(ctx, cl, req)
	case "dnsregistrations":
		err = h.registration(ctx, cl, req)
	case contributionResource:
		err = h.contribution(ctx, cl, req)
	case "dnszoneassociations":
		err = h.association(ctx, cl, req)
	case "dnsnamingpolicies":
		err = h.namingPolicy(ctx, cl, req)
	default:
		return kubeadmission.Allowed("project API authorization applies")
	}
	if err != nil {
		return kubeadmission.Denied(err.Error())
	}
	return kubeadmission.Allowed("DNS references are valid")
}

func (h *Handler) resolverAccess(ctx context.Context, cl client.Client, req kubeadmission.Request) error {
	var current dnsv1alpha1.DNSResolverAccessBinding
	if err := decode(req, &current); err != nil {
		return err
	}
	var resolverContext dnsv1alpha1.DNSResolverContext
	if err := cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: current.Spec.ContextRef.Name}, &resolverContext); err != nil {
		return fmt.Errorf("resolver context lookup: %w", err)
	}
	if resolverContext.UID != current.Spec.ContextRef.UID || !resolverContext.DeletionTimestamp.IsZero() {
		return fmt.Errorf("resolver context lifetime is not current")
	}
	ready := apimeta.FindStatusCondition(resolverContext.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != resolverContext.Generation {
		return fmt.Errorf("resolver context is not ready for its current generation")
	}
	return nil
}

func (h *Handler) contribution(ctx context.Context, cl client.Client, req kubeadmission.Request) error {
	var contribution dnsv1alpha1.DNSRecordContribution
	if err := decode(req, &contribution); err != nil {
		return err
	}
	var registration dnsv1alpha1.DNSRegistration
	if err := cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: contribution.Spec.RegistrationRef.Name}, &registration); err != nil {
		return err
	}
	if registration.UID != contribution.Spec.RegistrationRef.UID || registration.Generation != contribution.Spec.RegistrationRef.Generation || !registration.DeletionTimestamp.IsZero() {
		return fmt.Errorf("registration lifetime or generation is not current")
	}
	zone, err := currentPrivateZone(ctx, cl, req.Namespace, registration.Spec.DNSZoneRef)
	if err != nil {
		return err
	}
	var owners []string
	for _, set := range contribution.Spec.RecordSets {
		for _, record := range set.Records {
			owners = append(owners, record.Name)
		}
	}
	if err := h.refuseReservedScope(req, zone, owners); err != nil {
		return err
	}
	for _, set := range contribution.Spec.RecordSets {
		if !containsType(registration.Spec.RecordTypes, set.RecordType) {
			return fmt.Errorf("record type is outside the registration")
		}
		for _, record := range set.Records {
			if err := model.ValidatePrivateRecordOwner(record.Name, zone.Spec.DomainName, string(set.RecordType)); err != nil {
				return err
			}
			if !registrationCoversOwner(&registration, record.Name, zone.Spec.DomainName) {
				return fmt.Errorf("record owner %q is outside the registration", record.Name)
			}
		}
	}
	return nil
}

// registrationCoversOwner mirrors the publisher's name scope so writers get
// immediate feedback: an owner must be the registered name or fall within one
// of its reserved descendants.
func registrationCoversOwner(registration *dnsv1alpha1.DNSRegistration, owner, apex string) bool {
	name := zoneRelativeOwner(owner, apex)
	if name == zoneRelativeOwner(registration.Spec.Name, apex) {
		return true
	}
	for _, scope := range registration.Spec.ReservedDescendants {
		s := zoneRelativeOwner(scope, apex)
		if name == s || strings.HasSuffix(name, "."+s) {
			return true
		}
	}
	return false
}

func zoneRelativeOwner(name, apex string) string {
	n := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	a := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(apex), "."))
	if n == a {
		return "@"
	}
	return strings.TrimSuffix(n, "."+a)
}

func (h *Handler) association(ctx context.Context, cl client.Client, req kubeadmission.Request) error {
	var a dnsv1alpha1.DNSZoneAssociation
	if err := decode(req, &a); err != nil {
		return err
	}
	if err := validateNamedPrivateZone(ctx, cl, req.Namespace, a.Spec.DNSZoneRef); err != nil {
		return err
	}

	// Readiness is not required here: the context controller creates the
	// managed zone association before the context itself can become Ready.
	var resolverContext dnsv1alpha1.DNSResolverContext
	if err := cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: a.Spec.ResolverContextRef.Name}, &resolverContext); err != nil {
		return fmt.Errorf("resolver context lookup: %w", err)
	}
	if !resolverContext.DeletionTimestamp.IsZero() {
		return fmt.Errorf("resolver context is being deleted")
	}
	return h.refuseZoneInsideReservedScope(ctx, cl, req.Namespace, a.Spec.DNSZoneRef.Name, &resolverContext)
}

func (h *Handler) refuseZoneInsideReservedScope(ctx context.Context, cl client.Client, namespace, zoneName string, resolverContext *dnsv1alpha1.DNSResolverContext) error {
	var zone dnsv1alpha1.DNSZone
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: zoneName}, &zone); err != nil {
		return err
	}
	if resolverContext.Spec.ManagedNamespace.Enabled && h.ReservedScopes == nil {
		return fmt.Errorf("reservedScopes is not configured; use the configured internal-DNS admission endpoint")
	}
	if h.ReservedScopes.Shadows(&zone, resolverContext) {
		return fmt.Errorf("ReservedScope: zone shadows a reserved subtree in this resolver context")
	}
	return nil
}

func (h *Handler) namingPolicy(ctx context.Context, cl client.Client, req kubeadmission.Request) error {
	var p dnsv1alpha1.DNSNamingPolicy
	if err := decode(req, &p); err != nil {
		return err
	}

	if p.Spec.DNSZoneRef.Name != "" {
		if err := validateNamedPrivateZone(ctx, cl, req.Namespace, p.Spec.DNSZoneRef); err != nil {
			return err
		}
	}
	for _, rule := range p.Spec.AdditionalNames {
		if _, err := metav1.LabelSelectorAsSelector(rule.Selector.AsLabelSelector()); err != nil {
			return fmt.Errorf("invalid naming selector: %w", err)
		}
		if err := validateNamedPrivateZone(ctx, cl, req.Namespace, rule.DNSZoneRef); err != nil {
			return err
		}
	}

	var resolverContext dnsv1alpha1.DNSResolverContext
	if err := cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: p.Spec.ResolverContextRef.Name}, &resolverContext); err != nil {
		return fmt.Errorf("resolver context lookup: %w", err)
	}
	ready := apimeta.FindStatusCondition(resolverContext.Status.Conditions, "Ready")
	if !resolverContext.DeletionTimestamp.IsZero() || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != resolverContext.Generation {
		return fmt.Errorf("resolver context lifetime is not ready")
	}
	return nil
}

func validateNamedPrivateZone(ctx context.Context, cl client.Client, namespace string, ref dnsv1alpha1.DNSNameReference) error {
	if ref.Name == "" {
		return fmt.Errorf("zone reference requires a name")
	}
	var zone dnsv1alpha1.DNSZone
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &zone); err != nil {
		return fmt.Errorf("private zone lookup: %w", err)
	}
	if !zone.DeletionTimestamp.IsZero() || zone.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate {
		return fmt.Errorf("internal references require a live private zone")
	}
	return nil
}

func decode(req kubeadmission.Request, obj any) error {
	data := req.Object.Raw
	if req.Operation == admissionv1.Delete {
		data = req.OldObject.Raw
	}
	if len(data) == 0 {
		return fmt.Errorf("admission object is missing")
	}
	return json.Unmarshal(data, obj)
}

func containsType(values []dnsv1alpha1.RRType, value dnsv1alpha1.RRType) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func (h *Handler) registration(ctx context.Context, cl client.Client, req kubeadmission.Request) error {
	var registration dnsv1alpha1.DNSRegistration
	if err := decode(req, &registration); err != nil {
		return err
	}

	zone, err := currentPrivateZone(ctx, cl, req.Namespace, registration.Spec.DNSZoneRef)
	if err != nil {
		return err
	}
	for _, rt := range registration.Spec.RecordTypes {
		if err := model.ValidatePrivateRecordOwner(registration.Spec.Name, zone.Spec.DomainName, string(rt)); err != nil {
			return err
		}
	}
	for _, name := range registration.Spec.ReservedDescendants {
		for _, scope := range h.ReservedScopes {
			if model.IsManagedZone(zone) && model.Within(scope.Name, model.ZoneRelativeOwner(name, zone.Spec.DomainName)) && !slices.Contains(scope.Publishers, req.UserInfo.Username) {
				return fmt.Errorf("ReservedScope: claim overlaps reserved subtree %q", scope.Name)
			}
		}
	}
	names := append([]string{registration.Spec.Name}, registration.Spec.ReservedDescendants...)
	return h.refuseReservedScope(req, zone, names)
}

func (h *Handler) refuseReservedScope(req kubeadmission.Request, zone *dnsv1alpha1.DNSZone, owners []string) error {
	if model.IsManagedZone(zone) && h.ReservedScopes == nil {
		return fmt.Errorf("reservedScopes is not configured; use the configured internal-DNS admission endpoint")
	}
	for _, owner := range owners {
		if err := h.ReservedScopes.Authorize(zone, owner, req.UserInfo.Username); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) recordSet(ctx context.Context, cl client.Client, req kubeadmission.Request) error {
	var rs dnsv1alpha1.DNSRecordSet
	if err := decode(req, &rs); err != nil {
		return err
	}
	var zone dnsv1alpha1.DNSZone
	if err := cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: rs.Spec.DNSZoneRef.Name}, &zone); err != nil {
		return err
	}
	if zone.Spec.Visibility == dnsv1alpha1.DNSZoneVisibilityPrivate && model.IsManagedZone(&zone) && h.ReservedScopes == nil {
		return fmt.Errorf("reservedScopes is not configured; use the configured internal-DNS admission endpoint")
	}
	for _, record := range rs.Spec.Records {
		if scope := h.ReservedScopes.Match(&zone, record.Name); scope != nil {
			return fmt.Errorf("ReservedScope: %q is reserved for registered publishers", scope.Name)
		}
	}
	return nil
}

func currentPrivateZone(ctx context.Context, cl client.Client, namespace string, ref dnsv1alpha1.DNSNameReference) (*dnsv1alpha1.DNSZone, error) {
	if ref.Name == "" {
		return nil, fmt.Errorf("zone reference requires a name")
	}
	var zone dnsv1alpha1.DNSZone
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &zone); err != nil {
		return nil, fmt.Errorf("private zone lookup: %w", err)
	}
	if !zone.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("zone lifetime is not current")
	}
	if zone.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate {
		return nil, fmt.Errorf("internal references require a private zone")
	}
	return &zone, nil
}
