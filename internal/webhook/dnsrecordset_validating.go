// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	pdnsclient "go.miloapis.com/dns-operator/internal/dns/pdns"
)

// +kubebuilder:webhook:path=/validate-dns-networking-miloapis-com-v1alpha1-dnsrecordset,mutating=false,failurePolicy=ignore,sideEffects=None,groups=dns.networking.miloapis.com,resources=dnsrecordsets,verbs=create;update,versions=v1alpha1,name=vdnsrecordset.kb.io,admissionReviewVersions=v1

type DNSRecordSetValidator struct {
	Manager mcmanager.Manager
	Client  client.Client
}

var _ admission.Validator[*dnsv1alpha1.DNSRecordSet] = &DNSRecordSetValidator{}

func (v *DNSRecordSetValidator) ValidateCreate(ctx context.Context, rs *dnsv1alpha1.DNSRecordSet) (admission.Warnings, error) {
	return nil, v.refuseClaimedOwnerNames(ctx, nil, rs)
}

func (v *DNSRecordSetValidator) ValidateUpdate(ctx context.Context, oldRS, newRS *dnsv1alpha1.DNSRecordSet) (admission.Warnings, error) {
	return nil, v.refuseClaimedOwnerNames(ctx, oldRS, newRS)
}

func (v *DNSRecordSetValidator) ValidateDelete(context.Context, *dnsv1alpha1.DNSRecordSet) (admission.Warnings, error) {
	return nil, nil
}

func (v *DNSRecordSetValidator) refuseClaimedOwnerNames(ctx context.Context, oldRS, rs *dnsv1alpha1.DNSRecordSet) error {
	if rs.Spec.DNSZoneRef.Name == "" || len(rs.Spec.Records) == 0 {
		return nil
	}

	logger := logf.FromContext(ctx)
	cl := clusterClient(ctx, v.Manager, v.Client)

	var zone dnsv1alpha1.DNSZone
	key := types.NamespacedName{Namespace: rs.Namespace, Name: rs.Spec.DNSZoneRef.Name}
	if err := cl.Get(ctx, key, &zone); err != nil {
		logger.V(1).Info("skipping owner name conflict check; DNSZone lookup failed",
			"zone", key.String(), "error", err)
		return nil
	}

	claims := newOwnerClaims(rs, zone.Spec.DomainName)
	if oldRS != nil &&
		oldRS.Spec.RecordType == rs.Spec.RecordType &&
		oldRS.Spec.DNSZoneRef.Name == rs.Spec.DNSZoneRef.Name {
		for _, rec := range oldRS.Spec.Records {
			delete(claims, qualifiedOwnerKey(rec.Name, zone.Spec.DomainName))
		}
	}
	if len(claims) == 0 {
		return nil
	}

	var list dnsv1alpha1.DNSRecordSetList
	if err := cl.List(ctx, &list, client.InNamespace(rs.Namespace)); err != nil {
		logger.V(1).Info("skipping owner name conflict check; DNSRecordSet list failed",
			"namespace", rs.Namespace, "error", err)
		return nil
	}

	holders := map[string]*dnsv1alpha1.DNSRecordSet{}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == rs.Name || !other.DeletionTimestamp.IsZero() {
			continue
		}
		if other.Spec.DNSZoneRef.Name != rs.Spec.DNSZoneRef.Name ||
			other.Spec.RecordType != rs.Spec.RecordType {
			continue
		}
		for _, rec := range other.Spec.Records {
			k := qualifiedOwnerKey(rec.Name, zone.Spec.DomainName)
			if _, wanted := claims[k]; !wanted {
				continue
			}
			if held, ok := holders[k]; !ok || firstClaimant(held, other) == other {
				holders[k] = other
			}
		}
	}
	if len(holders) == 0 {
		return nil
	}

	contested := make([]string, 0, len(holders))
	for k := range holders {
		contested = append(contested, k)
	}
	sort.Slice(contested, func(i, j int) bool {
		return claims[contested[i]].recordIndex < claims[contested[j]].recordIndex
	})

	var errs field.ErrorList
	for _, k := range contested {
		claim := claims[k]
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "records").Index(claim.recordIndex).Child("name"),
			claim.ownerName,
			fmt.Sprintf(
				"owner name %q is already claimed by DNSRecordSet %q for record type %s in zone %q; "+
					"a name is held by one DNSRecordSet only, so this record would never be published",
				k, holders[k].Name, rs.Spec.RecordType, rs.Spec.DNSZoneRef.Name),
		))
	}

	return apierrors.NewInvalid(
		dnsv1alpha1.GroupVersion.WithKind("DNSRecordSet").GroupKind(), rs.Name, errs)
}

type ownerClaim struct {
	recordIndex int
	ownerName   string
}

func newOwnerClaims(rs *dnsv1alpha1.DNSRecordSet, zoneDomainName string) map[string]ownerClaim {
	claims := make(map[string]ownerClaim, len(rs.Spec.Records))
	for i, rec := range rs.Spec.Records {
		if rec.Name == "" {
			continue
		}
		k := qualifiedOwnerKey(rec.Name, zoneDomainName)
		if _, ok := claims[k]; ok {
			continue
		}
		claims[k] = ownerClaim{recordIndex: i, ownerName: rec.Name}
	}
	return claims
}

func qualifiedOwnerKey(ownerName, zoneDomainName string) string {
	return strings.ToLower(pdnsclient.QualifyOwner(ownerName, zoneDomainName))
}

func firstClaimant(a, b *dnsv1alpha1.DNSRecordSet) *dnsv1alpha1.DNSRecordSet {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		if a.Name <= b.Name {
			return a
		}
		return b
	}
	if a.CreationTimestamp.Before(&b.CreationTimestamp) {
		return a
	}
	return b
}
