// SPDX-License-Identifier: AGPL-3.0-only

// Package admission enforces the authenticated writer boundary for internal DNS.
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
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	kubeadmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const Path = "/validate-internal-dns"

// Handler uses an uncached project client for grant and registration lookups.
// TrustedClusterUID is supplied by the server configuration, never by a record
// publisher. For project-aware deployments, ClientForRequest must resolve the
// authenticated project context and return its immutable source cluster UID.
type Handler struct {
	Client                  client.Client
	TrustedClusterUID       string
	PlatformSubjects        []string
	IntegrationSubjects     []string
	MaxContributionLease    time.Duration
	MaxAccessLease          time.Duration
	ResolverContextsEnabled bool
	Now                     func() time.Time
	ClientForRequest        func(context.Context, kubeadmission.Request) (client.Client, string, error)
}

var _ kubeadmission.Handler = (*Handler)(nil)

func (h *Handler) Handle(ctx context.Context, req kubeadmission.Request) kubeadmission.Response {
	if req.Resource.Group != dnsv1alpha1.GroupVersion.Group {
		return kubeadmission.Denied("unexpected API group")
	}
	// Public-zone admission remains owned by the existing DNS operator. This
	// webhook must not give the internal service exclusive authority over the
	// public controller's status writes.
	if req.Resource.Resource == "dnszones" {
		var zone dnsv1alpha1.DNSZone
		if err := decode(req, &zone); err != nil {
			return kubeadmission.Denied(err.Error())
		}
		if zone.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate {
			return kubeadmission.Allowed("public DNS admission applies")
		}
	}
	platform := contains(h.PlatformSubjects, req.UserInfo.Username)
	integration := contains(h.IntegrationSubjects, req.UserInfo.Username)
	if req.Resource.Resource == "dnsresolvercontexts" || req.Resource.Resource == "dnsresolveraccessbindings" {
		if req.SubResource == "status" {
			if !platform {
				return kubeadmission.Denied("resolver context status is DNS platform-owned")
			}
			return kubeadmission.Allowed("platform writer")
		}
		if !integration {
			return kubeadmission.Denied("resolver context and access specs require a trusted integration writer")
		}
		cl := h.Client
		if h.ClientForRequest != nil {
			var err error
			cl, _, err = h.ClientForRequest(ctx, req)
			if err != nil {
				return kubeadmission.Errored(http.StatusServiceUnavailable, err)
			}
		}
		if cl == nil {
			return kubeadmission.Errored(http.StatusServiceUnavailable, fmt.Errorf("project API client unavailable"))
		}
		if req.Operation == admissionv1.Delete {
			return kubeadmission.Allowed("integration revocation")
		}
		var err error
		if req.Resource.Resource == "dnsresolvercontexts" {
			err = h.resolverContext(req)
		} else {
			err = h.resolverAccess(ctx, cl, req)
		}
		if err != nil {
			return kubeadmission.Denied(err.Error())
		}
		return kubeadmission.Allowed("trusted integration writer")
	}
	if platformResource(req.Resource.Resource) {
		if !platform {
			return kubeadmission.Denied("internal serving and publication resources require a platform writer")
		}
		return kubeadmission.Allowed("platform writer")
	}
	if req.SubResource == "status" && req.Resource.Resource != "dnsrecordcontributions" && !platform {
		return kubeadmission.Denied("DNS publication and writer authority status is platform-owned")
	}
	if platform {
		return kubeadmission.Allowed("platform writer; compiler validates publication inputs")
	}
	// Deletion revokes intent and cannot introduce serving authority. The API
	// server has already authorized delete on this exact project resource.
	// Requiring a live grant/registration here would strand retired objects and
	// block garbage collection after their parents have been deleted.
	if req.Operation == admissionv1.Delete {
		return kubeadmission.Allowed("project deletion authorization applies")
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
	case "dnsregistrations":
		err = h.registration(ctx, cl, req)
	case "dnsrecordcontributions":
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

func (h *Handler) resolverContext(req kubeadmission.Request) error {
	var current, old dnsv1alpha1.DNSResolverContext
	if err := decode(req, &current); err != nil {
		return err
	}
	if current.Spec.ConsumerID == "" {
		return fmt.Errorf("consumerID is required")
	}
	if len(req.OldObject.Raw) > 0 {
		if err := json.Unmarshal(req.OldObject.Raw, &old); err != nil {
			return err
		}
		if old.Spec.ConsumerID != current.Spec.ConsumerID {
			return fmt.Errorf("consumerID is immutable")
		}
		if !reflect.DeepEqual(old.Status, current.Status) {
			return fmt.Errorf("resolver context status is DNS platform-owned")
		}
	}
	return nil
}

func (h *Handler) resolverAccess(ctx context.Context, cl client.Client, req kubeadmission.Request) error {
	var current, old dnsv1alpha1.DNSResolverAccessBinding
	if err := decode(req, &current); err != nil {
		return err
	}
	a := current.Spec.Authorization
	if current.Spec.ContextRef.Name == "" || current.Spec.ContextRef.UID == "" {
		return fmt.Errorf("access binding must pin context name and UID")
	}
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
	if current.Spec.QueryIdentity.Type != dnsv1alpha1.DNSResolverQueryIdentityDestinationAddress {
		return fmt.Errorf("query identity type must be DestinationAddress")
	}
	if current.Spec.Region == "" || current.Spec.QueryIdentity.Value == "" || current.Spec.Port < 1 || a.WriterEpoch < 1 || a.Sequence < 1 || a.ValidUntil.IsZero() {
		return fmt.Errorf("region, destination, port, writer epoch, sequence, and deadline are required")
	}
	if current.Spec.Port != 53 || !validTransports(current.Spec.Transports) {
		return fmt.Errorf("access requires port 53 and exactly UDP and TCP")
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
	if len(req.OldObject.Raw) > 0 {
		if err := json.Unmarshal(req.OldObject.Raw, &old); err != nil {
			return err
		}
		if old.Spec.ContextRef != current.Spec.ContextRef || old.Spec.Region != current.Spec.Region || old.Spec.QueryIdentity != current.Spec.QueryIdentity || old.Spec.Port != current.Spec.Port || !reflect.DeepEqual(old.Spec.Transports, current.Spec.Transports) {
			return fmt.Errorf("access identity and transport are immutable; create a new binding")
		}
		if a.WriterEpoch != old.Spec.Authorization.WriterEpoch {
			return fmt.Errorf("writer epoch is immutable for an access binding lifetime")
		}
		if a.Sequence <= old.Spec.Authorization.Sequence {
			return fmt.Errorf("access renewal sequence must increase")
		}
		if !reflect.DeepEqual(old.Status, current.Status) {
			return fmt.Errorf("access binding status is DNS platform-owned")
		}
	}
	return nil
}

func validTransports(values []string) bool {
	if len(values) != 2 {
		return false
	}
	seen := map[string]bool{}
	for _, v := range values {
		if (v != "UDP" && v != "TCP") || seen[v] {
			return false
		}
		seen[v] = true
	}
	return seen["UDP"] && seen["TCP"]
}

func (h *Handler) contribution(ctx context.Context, cl client.Client, clusterUID string, req kubeadmission.Request) error {
	var c, old dnsv1alpha1.DNSRecordContribution
	if err := decode(req, &c); err != nil {
		return err
	}
	if c.Spec.GrantRef.UID == "" || c.Spec.RegistrationRef.UID == "" || c.Spec.RegistrationRef.Generation < 1 {
		return fmt.Errorf("contribution must pin grant UID and registration UID/generation")
	}
	var g dnsv1alpha1.DNSContributionGrant
	if err := cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: c.Spec.GrantRef.Name}, &g); err != nil {
		return fmt.Errorf("grant lookup: %w", err)
	}
	if g.UID != c.Spec.GrantRef.UID || !g.DeletionTimestamp.IsZero() {
		return fmt.Errorf("grant lifetime is not current")
	}
	if clusterUID == "" || g.Spec.Principal.ClusterUID != clusterUID || g.Spec.Principal.Subject != req.UserInfo.Username {
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
	if len(req.OldObject.Raw) > 0 {
		if err := json.Unmarshal(req.OldObject.Raw, &old); err != nil {
			return err
		}
		if old.Spec.GrantRef != c.Spec.GrantRef || old.Spec.RegistrationRef != c.Spec.RegistrationRef {
			return fmt.Errorf("contribution authority references are immutable; create a new contribution for a new policy lifetime")
		}
	}
	if req.SubResource == "status" {
		if !reflect.DeepEqual(old.Status.Conditions, c.Status.Conditions) || old.Status.PublishedRevision != c.Status.PublishedRevision {
			return fmt.Errorf("DNS publication status fields are platform-owned")
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
	if g.Spec.Principal.Subject == "" || g.Spec.Principal.ClusterUID == "" {
		return fmt.Errorf("producer principal is required")
	}
	if g.Spec.RegistrationRef.UID == "" {
		return fmt.Errorf("grant must pin its registration UID")
	}
	var r dnsv1alpha1.DNSRegistration
	if err := cl.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: g.Spec.RegistrationRef.Name}, &r); err != nil {
		return err
	}
	if r.UID != g.Spec.RegistrationRef.UID {
		return fmt.Errorf("registration lifetime is not current")
	}
	return authorize(ctx, cl, req.UserInfo, req.Namespace, dnsv1alpha1.GroupVersion.Group, "dnsregistrations", r.Name, "update")
}

func (h *Handler) association(ctx context.Context, cl client.Client, req kubeadmission.Request) error {
	var a dnsv1alpha1.DNSZoneAssociation
	if err := decode(req, &a); err != nil {
		return err
	}
	if _, err := currentPrivateZone(ctx, cl, req.Namespace, a.Spec.DNSZoneRef); err != nil {
		return err
	}
	if a.Spec.Managed {
		return fmt.Errorf("managed associations are platform-owned")
	}
	if err := validateConsumerSelector(a.Spec.VPCRef, a.Spec.ResolverContextRef, h.ResolverContextsEnabled); err != nil {
		return err
	}
	if err := authorize(ctx, cl, req.UserInfo, req.Namespace, dnsv1alpha1.GroupVersion.Group, "dnszones", a.Spec.DNSZoneRef.Name, "update"); err != nil {
		return err
	}
	if h.ResolverContextsEnabled {
		if a.Spec.ResolverContextRef.UID == "" {
			return fmt.Errorf("association must pin resolver context UID")
		}
		return authorize(ctx, cl, req.UserInfo, req.Namespace, dnsv1alpha1.GroupVersion.Group, "dnsresolvercontexts", a.Spec.ResolverContextRef.Name, "use")
	}
	return authorize(ctx, cl, req.UserInfo, req.Namespace, "networking.datumapis.com", "networks", a.Spec.VPCRef.Name, "use")
}

func (h *Handler) namingPolicy(ctx context.Context, cl client.Client, req kubeadmission.Request) error {
	var p dnsv1alpha1.DNSNamingPolicy
	if err := decode(req, &p); err != nil {
		return err
	}
	if err := validateConsumerSelector(p.Spec.VPCRef, p.Spec.ResolverContextRef, h.ResolverContextsEnabled); err != nil {
		return err
	}
	if p.Spec.DNSZoneRef.Name != "" {
		if _, err := currentPrivateZone(ctx, cl, req.Namespace, p.Spec.DNSZoneRef); err != nil {
			return err
		}
		if err := authorize(ctx, cl, req.UserInfo, req.Namespace, dnsv1alpha1.GroupVersion.Group, "dnszones", p.Spec.DNSZoneRef.Name, "update"); err != nil {
			return err
		}
	}
	for _, rule := range p.Spec.AdditionalNames {
		if _, err := currentPrivateZone(ctx, cl, req.Namespace, rule.DNSZoneRef); err != nil {
			return err
		}
		if err := authorize(ctx, cl, req.UserInfo, req.Namespace, dnsv1alpha1.GroupVersion.Group, "dnszones", rule.DNSZoneRef.Name, "update"); err != nil {
			return err
		}
	}
	if h.ResolverContextsEnabled {
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
		return authorize(ctx, cl, req.UserInfo, req.Namespace, dnsv1alpha1.GroupVersion.Group, "dnsresolvercontexts", resolverContext.Name, "use")
	}
	return authorize(ctx, cl, req.UserInfo, req.Namespace, "networking.datumapis.com", "networks", p.Spec.VPCRef.Name, "use")
}

func validateConsumerSelector(vpcRef, contextRef dnsv1alpha1.DNSObjectReference, contextMode bool) error {
	hasVPC := vpcRef.Name != "" || vpcRef.UID != "" || vpcRef.Generation != 0
	hasContext := contextRef.Name != "" || contextRef.UID != "" || contextRef.Generation != 0
	if hasVPC == hasContext {
		return fmt.Errorf("exactly one of vpcRef or resolverContextRef is required")
	}
	selected := vpcRef
	if hasContext {
		selected = contextRef
	}
	if selected.Name == "" || selected.UID == "" {
		return fmt.Errorf("consumer reference must pin name and UID")
	}
	if contextMode && !hasContext {
		return fmt.Errorf("resolver context mode requires resolverContextRef")
	}
	if !contextMode && !hasVPC {
		return fmt.Errorf("legacy networking mode requires vpcRef")
	}
	return nil
}

func authorize(ctx context.Context, cl client.Client, user authenticationv1.UserInfo, namespace, group, resource, name, verb string) error {
	if name == "" {
		return fmt.Errorf("authorization target name is required")
	}
	extra := map[string]authorizationv1.ExtraValue{}
	for k, v := range user.Extra {
		extra[k] = authorizationv1.ExtraValue(v)
	}
	review := &authorizationv1.SubjectAccessReview{ObjectMeta: metav1.ObjectMeta{}, Spec: authorizationv1.SubjectAccessReviewSpec{
		User: user.Username, UID: user.UID, Groups: user.Groups, Extra: extra,
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Group: group, Resource: resource, Name: name, Verb: verb},
	}}
	if err := cl.Create(ctx, review); err != nil {
		return fmt.Errorf("authorization review unavailable: %w", err)
	}
	if !review.Status.Allowed {
		return fmt.Errorf("caller is not authorized to %s %s/%s", verb, resource, name)
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

func platformResource(resource string) bool {
	switch resource {
	case "dnsresolverbindings", "dnspublicationmanifests", "dnspublicationchunks", "dnstransportoutboxes", "dnspublicationownerships", "dnsmanagednamespaces":
		return true
	default:
		return false
	}
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
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
	if registration.Spec.TTLSeconds < 1 || registration.Spec.TTLSeconds > dnsv1alpha1.DNSRegistrationMaxTTLSeconds {
		return fmt.Errorf("dynamic TTL must be between 1 and %d seconds", dnsv1alpha1.DNSRegistrationMaxTTLSeconds)
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
