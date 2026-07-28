// SPDX-License-Identifier: AGPL-3.0-only

// Package webhook provides admission webhooks for DNS resources.
//
// +kubebuilder:webhook:path=/mutate-dns-networking-miloapis-com-v1alpha1-dnsrecordset,mutating=true,failurePolicy=ignore,sideEffects=None,groups=dns.networking.miloapis.com,resources=dnsrecordsets,verbs=create;update,versions=v1alpha1,name=mdnsrecordset.kb.io,admissionReviewVersions=v1
package webhook

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/display"
)

// DNSRecordSetMutator stamps display-name / display-value annotations at
// admission so ActivityPolicy create audits can include the FQDN.
type DNSRecordSetMutator struct {
	Client client.Client
}

var _ admission.CustomDefaulter = &DNSRecordSetMutator{}

// Default looks up the referenced DNSZone and sets display annotations.
// Missing zones are non-fatal: annotations stay unset and ActivityPolicy
// fallbacks still include the relative owner name.
func (m *DNSRecordSetMutator) Default(ctx context.Context, obj runtime.Object) error {
	rs, ok := obj.(*dnsv1alpha1.DNSRecordSet)
	if !ok {
		return fmt.Errorf("expected DNSRecordSet, got %T", obj)
	}
	if rs.Spec.DNSZoneRef.Name == "" {
		return nil
	}

	var zone dnsv1alpha1.DNSZone
	key := types.NamespacedName{Namespace: rs.Namespace, Name: rs.Spec.DNSZoneRef.Name}
	if err := m.Client.Get(ctx, key, &zone); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting DNSZone %s/%s: %w", key.Namespace, key.Name, err)
	}

	_ = display.EnsureAnnotations(rs, zone.Spec.DomainName)
	return nil
}

// SetupDNSRecordSetWebhook registers the mutating webhook with the manager.
func SetupDNSRecordSetWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&dnsv1alpha1.DNSRecordSet{}).
		WithDefaulter(&DNSRecordSetMutator{Client: mgr.GetClient()}).
		Complete()
}
