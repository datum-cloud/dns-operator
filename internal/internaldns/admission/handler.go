// SPDX-License-Identifier: AGPL-3.0-only

// Package admission validates live references between private DNS resources.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

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

const Path = "/validate-internal-dns"

// Handler uses an uncached client selected by authenticated project context.
type Handler struct {
	Client client.Client

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
	switch req.Resource.Resource {
	case "dnsregistrations", contributionResource, "dnszoneassociations", "dnsnamingpolicies", "dnsresolveraccessbindings":
		// These resources require live references.
	default:
		return kubeadmission.Allowed("API server authorization and schema validation apply")
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
	for _, set := range contribution.Spec.RecordSets {
		if !containsType(registration.Spec.RecordTypes, set.RecordType) {
			return fmt.Errorf("record type is outside the registration")
		}
		for _, record := range set.Records {
			if err := model.ValidatePrivateRecordOwner(record.Name, zone.Spec.DomainName, string(set.RecordType)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *Handler) association(ctx context.Context, cl client.Client, req kubeadmission.Request) error {
	var a dnsv1alpha1.DNSZoneAssociation
	if err := decode(req, &a); err != nil {
		return err
	}
	if _, err := currentPrivateZone(ctx, cl, req.Namespace, a.Spec.DNSZoneRef); err != nil {
		return err
	}

	if a.Spec.ResolverContextRef.UID == "" {
		return fmt.Errorf("association must pin resolver context UID")
	}
	return nil
}

func (h *Handler) namingPolicy(ctx context.Context, cl client.Client, req kubeadmission.Request) error {
	var p dnsv1alpha1.DNSNamingPolicy
	if err := decode(req, &p); err != nil {
		return err
	}

	if p.Spec.DNSZoneRef.Name != "" {
		if _, err := currentPrivateZone(ctx, cl, req.Namespace, p.Spec.DNSZoneRef); err != nil {
			return err
		}
	}
	for _, rule := range p.Spec.AdditionalNames {
		if _, err := metav1.LabelSelectorAsSelector(rule.Selector.AsLabelSelector()); err != nil {
			return fmt.Errorf("invalid naming selector: %w", err)
		}
		if _, err := currentPrivateZone(ctx, cl, req.Namespace, rule.DNSZoneRef); err != nil {
			return err
		}
	}

	if p.Spec.ResolverContextRef.Name == "" || p.Spec.ResolverContextRef.UID == "" {
		return fmt.Errorf("naming policy must pin resolver context name and UID")
	}
	var resolverContext dnsv1alpha1.DNSResolverContext
	if err := cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: p.Spec.ResolverContextRef.Name}, &resolverContext); err != nil {
		return fmt.Errorf("resolver context lookup: %w", err)
	}
	ready := apimeta.FindStatusCondition(resolverContext.Status.Conditions, "Ready")
	if resolverContext.UID != p.Spec.ResolverContextRef.UID || !resolverContext.DeletionTimestamp.IsZero() || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != resolverContext.Generation {
		return fmt.Errorf("resolver context lifetime is not ready")
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
	return nil
}

// Zone references cannot silently follow a replacement with the same name.
func currentPrivateZone(ctx context.Context, cl client.Client, namespace string, ref dnsv1alpha1.DNSObjectReference) (*dnsv1alpha1.DNSZone, error) {
	if ref.Name == "" || ref.UID == "" {
		return nil, fmt.Errorf("zone reference must pin name and UID")
	}
	var zone dnsv1alpha1.DNSZone
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &zone); err != nil {
		return nil, fmt.Errorf("private zone lookup: %w", err)
	}
	if zone.UID != ref.UID || !zone.DeletionTimestamp.IsZero() || (ref.Generation != 0 && ref.Generation != zone.Generation) {
		return nil, fmt.Errorf("zone lifetime is not current")
	}
	if zone.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate {
		return nil, fmt.Errorf("internal references require a private zone")
	}
	return &zone, nil
}
