// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/projectstatus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// AckSink relies on NATS subject ACLs to bind each authenticated serving member
// to its own AckSubject. It also verifies membership, region, resource lifetime,
// revision and freshness before changing project-facing publication status.
type ProjectScope struct {
	Client         client.Client
	Namespace      string
	RequestTimeout time.Duration
}

type AckSink struct {
	Client             client.Client
	Namespace          string
	ProjectNamespaces  []string
	Projects           []ProjectScope
	PublicationMembers []model.Member
	PublicationRegions map[string][]model.Member
	Region             string
	Shard              string
	Members            []model.Member
	Now                func() time.Time
}

func (s *AckSink) Apply(ctx context.Context, subject string, env model.Envelope) error {
	if env.Kind != model.KindMemberAck || env.Region != s.Region || env.Shard != s.Shard {
		return fmt.Errorf("ack belongs to another kind, region or shard")
	}
	var a model.MemberAck
	if err := json.Unmarshal(env.Payload, &a); err != nil {
		return err
	}
	if subject != model.AckSubject(s.Region, s.Shard, a.MemberID) || env.ResourceUID != a.ResourceUID || env.Epoch != a.Epoch || env.Revision != a.Revision {
		return fmt.Errorf("ack subject or envelope identity mismatch")
	}
	var member *model.Member
	for i := range s.Members {
		if s.Members[i].MemberID == a.MemberID {
			member = &s.Members[i]
			break
		}
	}
	if member == nil || member.ReplicaID != a.ReplicaID {
		return fmt.Errorf("ack member identity is not assigned to this shard")
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	if a.ObservedAt.IsZero() || a.ObservedAt.After(now.Add(5*time.Second)) {
		return fmt.Errorf("ack clock is outside the allowed bound")
	}
	if a.Phase == model.AckVerified {
		if a.ValidUntil.IsZero() || a.ValidUntil.After(now.Add(2*time.Minute)) {
			return fmt.Errorf("verified ack requires a bounded fresh serving lease")
		}
		// A durable replay may deliver a correctly authenticated health lease
		// after its deadline. It can never make a member ready again, and
		// retrying it would create a permanent redelivery loop.
		if !a.ValidUntil.After(now) {
			return nil
		}
	}
	row := dnsv1alpha1.DNSApplyAcknowledgement{MemberID: a.MemberID, Phase: string(a.Phase), Revision: int64(a.Revision), ObservedAt: metav1.NewTime(a.ObservedAt), ObservedUnixNano: a.ObservedAt.UnixNano(), Error: a.Error}
	if !a.ValidUntil.IsZero() {
		until := metav1.NewTime(a.ValidUntil)
		row.ValidUntil = &until
	}
	switch a.Kind {
	case model.KindPublicationManifest:
		if member.Role != dnsValueRegional && member.Role != dnsValueCluster {
			return fmt.Errorf("publication ack is not from a regional resolver")
		}
		return s.publication(ctx, a, row, now)
	case model.KindServingSnapshot:
		if member.Role != "node" && member.Role != dnsValueCluster && member.Role != dnsValueResolver && member.Role != dnsValueRegional {
			return fmt.Errorf("serving ack is not from a resolver member")
		}
		return s.serving(ctx, a, row, now)
	default:
		return fmt.Errorf("unsupported acknowledged kind")
	}
}

func (s *AckSink) publication(ctx context.Context, a model.MemberAck, row dnsv1alpha1.DNSApplyAcknowledgement, now time.Time) error {
	var owner dnsv1alpha1.DNSPublicationOwnership
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: "owner-" + model.OpaqueToken(a.ResourceUID)[:20]}, &owner); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if string(owner.Spec.ZoneUID) != a.ResourceUID || owner.Spec.ActiveManifestName == "" {
		return nil
	}
	var m dnsv1alpha1.DNSPublicationManifest
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: owner.Spec.ActiveManifestName}, &m); err != nil {
		return err
	}
	if uint64(m.Spec.WriterEpoch) != a.Epoch || uint64(m.Spec.Revision) != a.Revision {
		return nil
	}
	row.WriterEpoch = int64(a.Epoch)
	base := m.DeepCopy()
	m.Status.ReplicaAcknowledgements = upsertAck(m.Status.ReplicaAcknowledgements, row)
	complete := s.publicationVerified(&m, now)
	condition(&m.Status.Conditions, "Published", complete, "ReplicaVerification", m.Generation, now)
	if err := s.Client.Status().Patch(ctx, &m, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	// Project-facing status is projected by Refresh. Keeping source API calls
	// out of this path lets the shared regional consumer durably acknowledge
	// fleet health even while one source control plane is partitioned.
	return nil
}

func (s *AckSink) serving(ctx context.Context, a model.MemberAck, row dnsv1alpha1.DNSApplyAcknowledgement, now time.Time) error {
	e, snapshot, err := LoadCommittedServingSnapshot(ctx, s.Client, s.Namespace, s.Region, s.Shard)
	if err != nil {
		return err
	}
	if e == nil || e.Epoch != a.Epoch || e.Revision != a.Revision {
		return nil
	}
	var bindings dnsv1alpha1.DNSResolverBindingList
	if err := s.Client.List(ctx, &bindings, client.InNamespace(s.Namespace)); err != nil {
		return err
	}
	for _, b := range snapshot.Bindings {
		if b.BindingUID != a.ResourceUID {
			continue
		}
		for i := range bindings.Items {
			binding := &bindings.Items[i]
			if string(binding.UID) != b.BindingUID || binding.Spec.Configuration.Generation != int64(b.BindingGeneration) || binding.Spec.Configuration.Revision != int64(b.ConfigurationRevision) || binding.Spec.Authorization.Sequence != int64(b.Authorization.Revision) || binding.Spec.Authorization.WriterEpoch != int64(b.Authorization.IssuerEpoch) {
				continue
			}
			base := binding.DeepCopy()
			bindingRow := row
			bindingRow.Revision = binding.Spec.Configuration.Revision
			bindingRow.WriterEpoch = int64(a.Epoch)
			bindingRow.SnapshotRevision = int64(a.Revision)
			bindingRow.AuthorizationIssuerEpoch = binding.Spec.Authorization.WriterEpoch
			bindingRow.AuthorizationRevision = binding.Spec.Authorization.Sequence
			if bindingRow.ValidUntil != nil && bindingRow.ValidUntil.After(b.Authorization.ValidUntil) {
				u := metav1.NewTime(b.Authorization.ValidUntil)
				bindingRow.ValidUntil = &u
			}
			binding.Status.MemberAcknowledgements = upsertAck(binding.Status.MemberAcknowledgements, bindingRow)
			complete := s.bindingVerified(binding, e, snapshot, now)
			if complete {
				binding.Status.Phase = "Serving"
				binding.Status.ObservedConfigurationRevision = binding.Spec.Configuration.Revision
			} else {
				binding.Status.Phase = dnsValuePending
			}
			condition(&binding.Status.Conditions, "Serving", complete, "MemberVerification", binding.Generation, now)
			if err := s.Client.Status().Patch(ctx, binding, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *AckSink) projectPublication(ctx context.Context, m *dnsv1alpha1.DNSPublicationManifest, complete bool, now time.Time) error {
	projects := append([]ProjectScope(nil), s.Projects...)
	for _, ns := range s.ProjectNamespaces {
		projects = append(projects, ProjectScope{Client: s.Client, Namespace: ns})
	}
	results := make(chan error, len(projects))
	for _, project := range projects {

		go func() {
			projectCtx := ctx
			cancel := func() {}
			if project.RequestTimeout > 0 {
				projectCtx, cancel = context.WithTimeout(ctx, project.RequestTimeout)
			}
			defer cancel()
			results <- s.projectPublicationForScope(projectCtx, project, m, complete, now)
		}()
	}
	var projectErrors error
	for range projects {
		projectErrors = errors.Join(projectErrors, <-results)
	}
	return projectErrors
}

func (s *AckSink) projectPublicationForScope(ctx context.Context, project ProjectScope, m *dnsv1alpha1.DNSPublicationManifest, complete bool, now time.Time) error {
	ns, projectClient := project.Namespace, project.Client
	if projectClient == nil {
		return fmt.Errorf("project %s status client unavailable", ns)
	}
	var projectErrors error
	{
		var zones dnsv1alpha1.DNSZoneList
		if err := projectClient.List(ctx, &zones, client.InNamespace(ns)); err != nil {
			return fmt.Errorf("project %s list zones: %w", ns, err)
		}
		for i := range zones.Items {
			z := &zones.Items[i]
			if z.UID != m.Spec.ZoneRef.UID {
				continue
			}
			base := z.DeepCopy()
			projectstatus.Published(&z.Status.Conditions, complete, z.Generation, now)
			projectstatus.Programmed(&z.Status.Conditions, hasApplied(m.Status.ReplicaAcknowledgements, m.Spec.WriterEpoch, m.Spec.Revision), z.Generation, now)
			if !jsonEqualStatus(base.Status, z.Status) {
				if err := projectClient.Status().Patch(ctx, z, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
					projectErrors = errors.Join(projectErrors, fmt.Errorf("project %s update zone %s status: %w", ns, z.Name, err))
				}
			}
			var regs dnsv1alpha1.DNSRegistrationList
			if err := projectClient.List(ctx, &regs, client.InNamespace(ns)); err != nil {
				projectErrors = errors.Join(projectErrors, fmt.Errorf("project %s list registrations: %w", ns, err))
				continue
			}
			var chunks []model.PublicationChunk
			for idx, ref := range m.Spec.Chunks {
				var c dnsv1alpha1.DNSPublicationChunk
				if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: ref.Name}, &c); err != nil {
					return err
				}
				chunks = append(chunks, model.PublicationChunk{ManifestUID: m.Name, ZoneUID: string(m.Spec.ZoneRef.UID), WriterEpoch: uint64(m.Spec.WriterEpoch), Revision: uint64(m.Spec.Revision), Index: idx, SHA256: c.Spec.SHA256, Payload: c.Spec.Payload})
			}
			manifest := model.PublicationManifest{ManifestUID: m.Name, ZoneUID: string(m.Spec.ZoneRef.UID), Apex: m.Spec.ZoneApex, WriterEpoch: uint64(m.Spec.WriterEpoch), Revision: uint64(m.Spec.Revision), Tombstone: m.Spec.Tombstone, ContentHash: m.Spec.ContentHash, GeneratedAt: m.Spec.GeneratedAt.Time}
			for idx, ref := range m.Spec.Chunks {
				manifest.Chunks = append(manifest.Chunks, model.ChunkRef{Index: idx, SHA256: ref.SHA256, Size: int(ref.Size)})
			}
			payload, err := model.VerifyManifest(manifest, chunks)
			if err != nil {
				return err
			}
			var plan model.PublicationPlan
			if len(payload) > 0 {
				if err := json.Unmarshal(payload, &plan); err != nil {
					return err
				}
			}
			registrationGenerations := map[string]uint64{}
			for _, fence := range plan.Registrations {
				registrationGenerations[fence.UID] = fence.Generation
			}
			for j := range regs.Items {
				r := &regs.Items[j]
				if registrationGenerations[string(r.UID)] != uint64(r.Generation) {
					continue
				}
				if r.Spec.DNSZoneRef.Name != z.Name || r.Spec.DNSZoneRef.UID != "" && r.Spec.DNSZoneRef.UID != z.UID {
					continue
				}
				available := false
				name := model.AbsoluteName(r.Spec.Name)
				apex := model.AbsoluteName(z.Spec.DomainName)
				if name == "@." {
					name = apex
				} else if !strings.HasSuffix(name, "."+apex) && name != apex {
					name = strings.TrimSuffix(name, ".") + "." + apex
				}
				for _, rrset := range plan.RRSets {
					if model.AbsoluteName(rrset.Name) != name {
						continue
					}
					for _, record := range rrset.Records {
						if record.Eligible && (record.ContributionUID == "" || record.ValidUntil.After(now)) {
							available = true
						}
					}
				}
				base := r.DeepCopy()
				r.Status.PublicationWriterEpoch = m.Spec.WriterEpoch
				r.Status.PublicationRevision = m.Spec.Revision
				projectstatus.Published(&r.Status.Conditions, complete, r.Generation, now)
				projectstatus.Available(&r.Status.Conditions, available, r.Generation, now)
				if !jsonEqualStatus(base.Status, r.Status) {
					if err := projectClient.Status().Patch(ctx, r, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
						projectErrors = errors.Join(projectErrors, fmt.Errorf("project %s update registration %s status: %w", ns, r.Name, err))
					}
				}
			}
		}
	}
	return projectErrors
}

func (s *AckSink) verified(rows []dnsv1alpha1.DNSApplyAcknowledgement, now time.Time, role string, revision, epoch, authEpoch, authRevision int64) bool {
	matches := func(member model.Member) bool {
		for _, row := range rows {
			if row.MemberID == member.MemberID && row.Revision == revision && row.WriterEpoch == epoch && (role != dnsValueResolver || row.AuthorizationIssuerEpoch == authEpoch && row.AuthorizationRevision == authRevision) && row.Phase == string(model.AckVerified) && row.ValidUntil != nil && row.ValidUntil.After(now) {
				return true
			}
		}
		return false
	}
	if role == "publication" {
		groups := s.PublicationRegions
		if len(groups) == 0 {
			members := s.PublicationMembers
			if len(members) == 0 {
				members = s.Members
			}
			groups = map[string][]model.Member{"local": members}
		}
		for _, members := range groups {
			required := 0
			verified := 0
			for _, member := range members {
				if member.Role == dnsValueRegional || member.Role == dnsValueCluster {
					required++
					if matches(member) {
						verified++
					}
				}
			}
			if required == 0 || verified != required {
				return false
			}
		}
		return len(groups) > 0
	}
	required := 0
	for _, member := range s.Members {
		match := member.Role == role
		if role == dnsValueResolver {
			match = member.Role == "node" || member.Role == dnsValueCluster || member.Role == dnsValueResolver || member.Role == dnsValueRegional
		}
		if !match {
			continue
		}
		required++
		if !matches(member) {
			return false
		}
	}
	return required > 0
}

func upsertAck(rows []dnsv1alpha1.DNSApplyAcknowledgement, row dnsv1alpha1.DNSApplyAcknowledgement) []dnsv1alpha1.DNSApplyAcknowledgement {
	for i := range rows {
		if rows[i].MemberID == row.MemberID {
			if row.ObservedUnixNano > 0 && rows[i].ObservedUnixNano > 0 && row.ObservedUnixNano < rows[i].ObservedUnixNano || row.ObservedAt.Before(&rows[i].ObservedAt) {
				return rows
			}
			rows[i] = row
			return rows
		}
	}
	return append(rows, row)
}
func condition(rows *[]metav1.Condition, name string, value bool, reason string, generation int64, now time.Time) {
	status := metav1.ConditionFalse
	if value {
		status = metav1.ConditionTrue
	}
	apimeta.SetStatusCondition(rows, metav1.Condition{Type: name, Status: status, Reason: reason, Message: reason, ObservedGeneration: generation, LastTransitionTime: metav1.NewTime(now)})
}

func hasApplied(rows []dnsv1alpha1.DNSApplyAcknowledgement, epoch, revision int64) bool {
	for _, row := range rows {
		if row.WriterEpoch == epoch && row.Revision == revision && (row.Phase == string(model.AckApplied) || row.Phase == string(model.AckVerified)) {
			return true
		}
	}
	return false
}

// Refresh withdraws status readiness when a member lease or authorization expires,
// even if its process or event stream has stopped producing acknowledgements.
func (s *AckSink) Refresh(ctx context.Context) error {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	var bindings dnsv1alpha1.DNSResolverBindingList
	if err := s.Client.List(ctx, &bindings, client.InNamespace(s.Namespace), client.MatchingLabels{"internal-dns.miloapis.com/region": model.SafeToken(s.Region), "internal-dns.miloapis.com/shard": model.SafeToken(s.Shard)}); err != nil {
		return err
	}
	envelope, snapshot, err := LoadCommittedServingSnapshot(ctx, s.Client, s.Namespace, s.Region, s.Shard)
	if err != nil {
		return err
	}
	for i := range bindings.Items {
		b := &bindings.Items[i]
		base := b.DeepCopy()
		complete := s.bindingVerified(b, envelope, snapshot, now)
		condition(&b.Status.Conditions, "Serving", complete, "MemberVerification", b.Generation, now)
		if complete {
			b.Status.Phase = "Serving"
		} else {
			b.Status.Phase = dnsValuePending
		}
		if !jsonEqualStatus(base.Status, b.Status) {
			if err := s.Client.Status().Patch(ctx, b, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
	}
	var owners dnsv1alpha1.DNSPublicationOwnershipList
	if err := s.Client.List(ctx, &owners, client.InNamespace(s.Namespace)); err != nil {
		return err
	}
	type projection struct {
		manifest *dnsv1alpha1.DNSPublicationManifest
		complete bool
	}
	projections := make([]projection, 0, len(owners.Items))
	var refreshErrors error
	for _, owner := range owners.Items {
		if owner.Spec.ActiveManifestName == "" {
			continue
		}
		var m dnsv1alpha1.DNSPublicationManifest
		if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: owner.Spec.ActiveManifestName}, &m); err != nil {
			refreshErrors = errors.Join(refreshErrors, err)
			continue
		}
		base := m.DeepCopy()
		complete := s.publicationVerified(&m, now)
		condition(&m.Status.Conditions, "Published", complete, "ReplicaVerification", m.Generation, now)
		if !jsonEqualStatus(base.Status, m.Status) {
			if err := s.Client.Status().Patch(ctx, &m, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				refreshErrors = errors.Join(refreshErrors, err)
				continue
			}
		}
		manifest := m.DeepCopy()
		projections = append(projections, projection{manifest: manifest, complete: complete})
	}
	// Launch every manifest projection before waiting for any source. Thus one
	// partitioned project cannot delay status for healthy projects or manifests.
	results := make(chan error, len(projections))
	for _, p := range projections {

		go func() { results <- s.projectPublication(ctx, p.manifest, p.complete, now) }()
	}
	for range projections {
		refreshErrors = errors.Join(refreshErrors, <-results)
	}
	return refreshErrors
}

func jsonEqualStatus(a, b interface{}) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func (s *AckSink) publicationVerified(m *dnsv1alpha1.DNSPublicationManifest, now time.Time) bool {
	if len(m.Spec.ServingTargets) == 0 {
		return false
	}
	scoped := *s
	scoped.PublicationRegions = map[string][]model.Member{}
	for _, target := range m.Spec.ServingTargets {
		key := target.Region + "/" + target.Shard
		members, ok := s.PublicationRegions[key]
		if !ok && len(s.PublicationRegions) == 0 && target.Region == s.Region && target.Shard == s.Shard {
			members = s.Members
			ok = true
		}
		if !ok {
			return false
		}
		scoped.PublicationRegions[key] = members
	}
	return scoped.verified(m.Status.ReplicaAcknowledgements, now, "publication", m.Spec.Revision, m.Spec.WriterEpoch, 0, 0)
}

// LoadCommittedServingSnapshot reads the shard's durable commit pointer rather
// than accepting mutually consistent but retired member acknowledgements.
func LoadCommittedServingSnapshot(ctx context.Context, c client.Client, namespace, region, shard string) (*model.Envelope, *model.ServingSnapshot, error) {
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "dns-shard-owner-" + model.OpaqueToken(region+"/"+shard)}, &cm); apierrors.IsNotFound(err) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	var owner shardState
	if err := json.Unmarshal([]byte(cm.Data["state"]), &owner); err != nil {
		return nil, nil, err
	}
	if owner.ActiveOutbox == "" {
		return nil, nil, nil
	}
	var out dnsv1alpha1.DNSTransportOutbox
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: owner.ActiveOutbox}, &out); err != nil {
		return nil, nil, err
	}
	if model.Hash(out.Spec.Payload) != out.Spec.PayloadHash {
		return nil, nil, fmt.Errorf("committed serving snapshot hash mismatch")
	}
	var e model.Envelope
	if err := json.Unmarshal(out.Spec.Payload, &e); err != nil {
		return nil, nil, err
	}
	if e.Kind != model.KindServingSnapshot || e.Region != region || e.Shard != shard || e.Epoch != owner.Epoch || int64(e.Epoch) != out.Spec.WriterEpoch || int64(e.Revision) != out.Spec.Revision {
		return nil, nil, fmt.Errorf("committed serving snapshot fence mismatch")
	}
	var snapshot model.ServingSnapshot
	if err := json.Unmarshal(e.Payload, &snapshot); err != nil {
		return nil, nil, err
	}
	if snapshot.Region != region || snapshot.Shard != shard || snapshot.ConfigurationEpoch != e.Epoch || snapshot.ConfigurationRevision != e.Revision {
		return nil, nil, fmt.Errorf("committed serving snapshot body fence mismatch")
	}
	return &e, &snapshot, nil
}

func (s *AckSink) bindingVerified(b *dnsv1alpha1.DNSResolverBinding, envelope *model.Envelope, snapshot *model.ServingSnapshot, now time.Time) bool {
	if envelope == nil || snapshot == nil || b.Spec.Tombstone || !b.Spec.Authorization.ValidUntil.After(now) {
		return false
	}
	present := false
	for _, desired := range snapshot.Bindings {
		if desired.BindingUID == string(b.UID) && !desired.Tombstone && int64(desired.BindingGeneration) == b.Spec.Configuration.Generation && int64(desired.ConfigurationRevision) == b.Spec.Configuration.Revision && int64(desired.Authorization.IssuerEpoch) == b.Spec.Authorization.WriterEpoch && int64(desired.Authorization.Revision) == b.Spec.Authorization.Sequence {
			present = true
			break
		}
	}
	if !present {
		return false
	}
	rows := make([]dnsv1alpha1.DNSApplyAcknowledgement, 0, len(b.Status.MemberAcknowledgements))
	for _, row := range b.Status.MemberAcknowledgements {
		if row.SnapshotRevision == int64(envelope.Revision) {
			rows = append(rows, row)
		}
	}
	return s.verified(rows, now, dnsValueResolver, b.Spec.Configuration.Revision, int64(envelope.Epoch), b.Spec.Authorization.WriterEpoch, b.Spec.Authorization.Sequence)
}
