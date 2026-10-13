// SPDX-License-Identifier: AGPL-3.0-only

// Package admission validates live DNS references and publisher grant relationships.
// Compiler validation remains necessary, but cannot infer a Kubernetes caller
// from a stored object's spec. Install this webhook with failurePolicy: Fail.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"time"

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

// Handler uses an uncached project client for grant and registration lookups.
// TrustedClusterUID is supplied by the server configuration, never by a record
// publisher. For project-aware deployments, ClientForRequest must resolve the
// authenticated project context and return its immutable source cluster UID.
type Handler struct {
	Client               client.Client
	TrustedClusterUID    string
	MaxContributionLease time.Duration
	MaxAccessLease       time.Duration
	Now                  func() time.Time
	ClientForRequest     func(context.Context, kubeadmission.Request) (client.Client, string, error)
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
	if req.SubResource == subresourceStatus && req.Resource.Resource != contributionResource {
		return kubeadmission.Allowed("API server status authorization applies")
	}
	switch req.Resource.Resource {
	case "dnsregistrations", contributionResource, "dnscontributiongrants", "dnszoneassociations", "dnsnamingpolicies", "dnsresolveraccessbindings":
		// These resources require live references or bounded freshness checks.
	default:
		return kubeadmission.Allowed("API server authorization and schema validation apply")
	}
	cl, clusterUID := h.Client, h.TrustedClusterUID
	if h.ClientForRequest != nil {
		var err error
		cl, clusterUID, err = h.ClientForRequest(ctx, req)
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
		err = h.contribution(ctx, cl, clusterUID, req)
	case "dnscontributiongrants":
		err = h.grant(ctx, cl, req)
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
	return kubeadmission.Allowed("authenticated and scoped DNS writer")
}

func (h *Handler) resolverAccess(ctx context.Context, cl client.Client, req kubeadmission.Request) error {
	var current dnsv1alpha1.DNSResolverAccessBinding
	if err := decode(req, &current); err != nil {
		return err
	}
	a := current.Spec.Authorization
	var resolverContext dnsv1alpha1.DNSResolverContext
	if err := cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: current.Spec.ContextRef.Name}, &resolverContext); err != nil {
		return fmt.Errorf("resolver context lookup: %w", err)
	}
	if resolverContext.UID != current.Spec.ContextRef.UID || !resolverContext.DeletionTimestamp.IsZero() {
		return fmt.Errorf("resolver context lifetime is not current")
	}
	if resolverContext.Status.AccessWriterEpoch < 1 || current.Spec.Authorization.WriterEpoch != resolverContext.Status.AccessWriterEpoch {
		return fmt.Errorf("access writer epoch is retired or not issued")
	}
	ready := apimeta.FindStatusCondition(resolverContext.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != resolverContext.Generation {
		return fmt.Errorf("resolver context is not ready for its current generation")
	}
	max := h.MaxAccessLease
	if max <= 0 {
		max = 5 * time.Minute
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	if !a.ValidUntil.After(now) {
		return fmt.Errorf("access deadline is already expired")
	}
	if a.ValidUntil.After(now.Add(max)) {
		return fmt.Errorf("access deadline exceeds platform lease limit")
	}
	return nil
}

func (h *Handler) contribution(ctx context.Context, cl client.Client, clusterUID string, req kubeadmission.Request) error {
	var c, old dnsv1alpha1.DNSRecordContribution
	if err := decode(req, &c); err != nil {
		return err
	}
	if len(req.OldObject.Raw) > 0 {
		if err := json.Unmarshal(req.OldObject.Raw, &old); err != nil {
			return err
		}
	}
	bindEpoch := false
	if req.SubResource == subresourceStatus {
		previous, current := old.Status, c.Status
		previous.Conditions, current.Conditions = nil, nil
		previous.PublishedRevision, current.PublishedRevision = 0, 0
		if reflect.DeepEqual(previous, current) {
			return nil
		}
		// Binding an issued epoch cannot introduce or renew endpoint eligibility.
		if previous.WriterEpoch == 0 && previous.ObservedGeneration == 0 && previous.Sequence == 0 && !previous.Eligible && previous.ValidUntil == nil && current.WriterEpoch > 0 {
			current.WriterEpoch = 0
			bindEpoch = reflect.DeepEqual(previous, current)
		}
	}
	var g dnsv1alpha1.DNSContributionGrant
	if err := cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: c.Spec.GrantRef.Name}, &g); err != nil {
		return fmt.Errorf("grant lookup: %w", err)
	}
	if g.UID != c.Spec.GrantRef.UID || !g.DeletionTimestamp.IsZero() {
		return fmt.Errorf("grant lifetime is not current")
	}
	if clusterUID == "" || g.Spec.Principal.ClusterUID != clusterUID || (!bindEpoch && g.Spec.Principal.Subject != req.UserInfo.Username) {
		return fmt.Errorf("caller does not match the grant's authenticated producer principal")
	}
	if g.Spec.RegistrationRef.UID != c.Spec.RegistrationRef.UID || g.Spec.RegistrationRef.Name != c.Spec.RegistrationRef.Name {
		return fmt.Errorf("grant belongs to another registration")
	}
	var r dnsv1alpha1.DNSRegistration
	if err := cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: c.Spec.RegistrationRef.Name}, &r); err != nil {
		return fmt.Errorf("registration lookup: %w", err)
	}
	if r.UID != c.Spec.RegistrationRef.UID || r.Generation != c.Spec.RegistrationRef.Generation || !r.DeletionTimestamp.IsZero() {
		return fmt.Errorf("registration lifetime or policy generation is not current")
	}
	if g.Status.ActiveWriterEpoch < 1 || g.Status.ObservedRegistrationGeneration != r.Generation || g.Status.ObservedGrantGeneration != g.Generation {
		return fmt.Errorf("grant authority is not issued for the current registration generation")
	}
	var zone dnsv1alpha1.DNSZone
	if err := cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: r.Spec.DNSZoneRef.Name}, &zone); err != nil {
		return fmt.Errorf("private zone lookup: %w", err)
	}
	if zone.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate {
		return fmt.Errorf("internal contributions require a private zone")
	}
	for _, rrset := range c.Spec.RecordSets {
		for _, record := range rrset.Records {
			if err := model.ValidatePrivateRecordOwner(record.Name, zone.Spec.DomainName, string(rrset.RecordType)); err != nil {
				return err
			}
		}
		if !containsType(g.Spec.RecordTypes, rrset.RecordType) || !containsType(r.Spec.RecordTypes, rrset.RecordType) {
			return fmt.Errorf("record type %s is outside the writer's scope", rrset.RecordType)
		}
	}
	if bindEpoch {
		if c.Status.WriterEpoch != g.Status.ActiveWriterEpoch {
			return fmt.Errorf("writer epoch is retired or not issued")
		}
		return nil
	}
	if req.SubResource == subresourceStatus {
		if !reflect.DeepEqual(old.Status.Conditions, c.Status.Conditions) || old.Status.PublishedRevision != c.Status.PublishedRevision {
			return fmt.Errorf("publisher observations cannot change publication results")
		}
		if c.Status.WriterEpoch != g.Status.ActiveWriterEpoch {
			return fmt.Errorf("writer epoch is retired or not issued")
		}
		if c.Status.Sequence <= old.Status.Sequence {
			return fmt.Errorf("producer observation sequence must increase")
		}
		if c.Status.ObservedGeneration != c.Generation {
			return fmt.Errorf("producer observation must match the contribution generation")
		}
		if c.Status.ValidUntil == nil {
			return fmt.Errorf("producer observation requires an original freshness deadline")
		}
		now := time.Now()
		if h.Now != nil {
			now = h.Now()
		}
		maxLease := h.MaxContributionLease
		if maxLease <= 0 {
			maxLease = 90 * time.Second
		}
		if c.Status.ValidUntil.After(now.Add(maxLease)) {
			return fmt.Errorf("freshness deadline exceeds platform lease limit")
		}
		if c.Status.Eligible && !c.Status.ValidUntil.After(now) {
			return fmt.Errorf("eligible observation is already expired")
		}
	} else if req.Operation != admissionv1.Delete && !reflect.DeepEqual(old.Status, c.Status) && req.Operation != admissionv1.Create {
		return fmt.Errorf("producer observations must use the status subresource")
	}
	return nil
}

func (h *Handler) grant(ctx context.Context, cl client.Client, req kubeadmission.Request) error {
	var g dnsv1alpha1.DNSContributionGrant
	if err := decode(req, &g); err != nil {
		return err
	}
	var r dnsv1alpha1.DNSRegistration
	if err := cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: g.Spec.RegistrationRef.Name}, &r); err != nil {
		return err
	}
	if r.UID != g.Spec.RegistrationRef.UID {
		return fmt.Errorf("registration lifetime is not current")
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
