// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

// A member's liveness is one lease per member, renewed by any verified
// acknowledgement it sends. A manifest row only proves the member verified that
// revision, so it is rewritten when it changes or nears publicationRowLease,
// not on every renewal.
const (
	memberLeaseKey            = "validUntil"
	memberLeaseRenewAfter     = 15 * time.Second
	publicationRowLease       = 10 * time.Minute
	publicationRowRenewBefore = 5 * time.Minute
)

func memberLeaseName(region, shard, member string) string {
	return model.MemberLeasePrefix + model.OpaqueToken(region+"/"+shard+"/"+member)
}

func memberLeaseKeyFor(group, member string) string { return group + "/" + member }

// renewMemberLease extends the member's lease to until and reports whether it
// wrote. Renewals that extend it by less than memberLeaseRenewAfter are skipped.
func (s *AckSink) renewMemberLease(ctx context.Context, member string, until time.Time) (bool, error) {
	key := client.ObjectKey{Namespace: s.Namespace, Name: memberLeaseName(s.Region, s.Shard, member)}
	value := until.UTC().Format(time.RFC3339Nano)
	var cm corev1.ConfigMap
	if err := s.Client.Get(ctx, key, &cm); apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{
			"internal-dns.miloapis.com/region": model.SafeToken(s.Region),
			"internal-dns.miloapis.com/shard":  model.SafeToken(s.Shard),
		}}, Data: map[string]string{memberLeaseKey: value}}
		if err := s.Client.Create(ctx, &cm); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return false, apierrors.NewConflict(corev1.Resource("configmaps"), key.Name, err)
			}
			return false, err
		}
		return true, nil
	} else if err != nil {
		return false, err
	}
	if stored, err := time.Parse(time.RFC3339Nano, cm.Data[memberLeaseKey]); err == nil && until.Before(stored.Add(memberLeaseRenewAfter)) {
		return false, nil
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[memberLeaseKey] = value
	if err := s.Client.Update(ctx, &cm); err != nil {
		return false, err
	}
	return true, nil
}

// publicationGroups are the serving groups whose members verify publications,
// keyed region/shard.
func (s *AckSink) publicationGroups() map[string][]model.Member {
	if len(s.PublicationRegions) > 0 {
		return s.PublicationRegions
	}
	members := s.PublicationMembers
	if len(members) == 0 {
		members = s.Members
	}
	return map[string][]model.Member{s.Region + "/" + s.Shard: members}
}

// loadMemberLeases reads the leases of every publication-verifying member. A
// member without a lease is left out: its manifest rows alone decide.
func (s *AckSink) loadMemberLeases(ctx context.Context) (map[string]time.Time, error) {
	leases := map[string]time.Time{}
	for group, members := range s.publicationGroups() {
		region, shard := cutGroup(group)
		for _, member := range members {
			if member.Role != dnsValueRegional && member.Role != dnsValueCluster {
				continue
			}
			var cm corev1.ConfigMap
			err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: memberLeaseName(region, shard, member.MemberID)}, &cm)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if until, err := time.Parse(time.RFC3339Nano, cm.Data[memberLeaseKey]); err == nil {
				leases[memberLeaseKeyFor(group, member.MemberID)] = until
			}
		}
	}
	return leases, nil
}

func cutGroup(group string) (string, string) {
	i := strings.LastIndex(group, "/")
	if i < 0 {
		return group, ""
	}
	return group[:i], group[i+1:]
}

// publicationRowChanged reports whether row says something the stored rows do
// not: a new member, phase, fence or error, or a verification whose stored row
// is close to lapsing.
func publicationRowChanged(rows []dnsv1alpha1.DNSApplyAcknowledgement, row dnsv1alpha1.DNSApplyAcknowledgement, now time.Time) bool {
	for _, stored := range rows {
		if stored.MemberID != row.MemberID {
			continue
		}
		if stored.Phase != row.Phase || stored.Revision != row.Revision || stored.WriterEpoch != row.WriterEpoch || stored.Error != row.Error {
			return true
		}
		if row.Phase != string(model.AckVerified) {
			return false
		}
		return stored.ValidUntil == nil || stored.ValidUntil.Sub(now) < publicationRowRenewBefore
	}
	return true
}
