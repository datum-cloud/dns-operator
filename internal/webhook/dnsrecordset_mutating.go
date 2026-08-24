// SPDX-License-Identifier: AGPL-3.0-only

// Package webhook provides admission webhooks for DNS resources.
//
// +kubebuilder:webhook:path=/mutate-dns-networking-miloapis-com-v1alpha1-dnsrecordset,mutating=true,failurePolicy=fail,sideEffects=None,groups=dns.networking.miloapis.com,resources=dnsrecordsets,verbs=create;update,versions=v1alpha1,name=mdnsrecordset.kb.io,admissionReviewVersions=v1
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/display"
)

// DNSRecordSetMutator stamps display-name / display-value annotations at
// admission so ActivityPolicy create audits can include the FQDN. On update it
// also stamps activity-* annotations from a records[] diff (issue #72).
type DNSRecordSetMutator struct {
	// Manager is optional; when set and cluster context is present, DNSZone
	// lookups use the project control plane client.
	Manager mcmanager.Manager
	// Client is the local/deployment-cluster client used as fallback.
	Client client.Client
}

var _ admission.Defaulter[*dnsv1alpha1.DNSRecordSet] = &DNSRecordSetMutator{}

// Default looks up the referenced DNSZone and sets display annotations.
// Missing zones are non-fatal: annotations stay unset and ActivityPolicy
// fallbacks still include the relative owner name.
func (m *DNSRecordSetMutator) Default(ctx context.Context, rs *dnsv1alpha1.DNSRecordSet) error {
	if rs.Spec.DNSZoneRef.Name == "" {
		return nil
	}

	cl := m.clientForZoneLookup(ctx)
	var zone dnsv1alpha1.DNSZone
	key := types.NamespacedName{Namespace: rs.Namespace, Name: rs.Spec.DNSZoneRef.Name}
	if err := cl.Get(ctx, key, &zone); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting DNSZone %s/%s: %w", key.Namespace, key.Name, err)
	}

	_ = display.EnsureAnnotations(rs, zone.Spec.DomainName)
	m.stampActivityAnnotations(ctx, rs, zone.Spec.DomainName)
	return nil
}

// stampActivityAnnotations compares against admission OldObject when present
// (updates) and stamps or clears activity-change / activity-name / activity-value.
func (m *DNSRecordSetMutator) stampActivityAnnotations(ctx context.Context, rs *dnsv1alpha1.DNSRecordSet, zoneDomainName string) {
	req, err := admission.RequestFromContext(ctx)
	if err != nil || len(req.OldObject.Raw) == 0 {
		_ = display.ClearActivityAnnotations(rs)
		return
	}

	var oldRS dnsv1alpha1.DNSRecordSet
	if err := json.Unmarshal(req.OldObject.Raw, &oldRS); err != nil {
		logf.FromContext(ctx).V(1).Info("skipping activity annotations; failed to decode OldObject", "error", err)
		_ = display.ClearActivityAnnotations(rs)
		return
	}
	_ = display.EnsureActivityAnnotations(rs, &oldRS, zoneDomainName)
}

// clientForZoneLookup returns the project control plane client when cluster
// context is available, otherwise the local Client.
//
// milo v0.7.4 engages cluster-scoped Projects as req.String() ("/projectname"),
// while admission Extra carries the bare project name. Try both forms.
func (m *DNSRecordSetMutator) clientForZoneLookup(ctx context.Context) client.Client {
	if m.Manager == nil {
		return m.Client
	}
	clusterName, ok := mccontext.ClusterFrom(ctx)
	if !ok || clusterName == "" {
		return m.Client
	}

	cl, err := m.Manager.GetCluster(ctx, clusterName)
	if err != nil && !strings.HasPrefix(clusterName.String(), "/") {
		cl, err = m.Manager.GetCluster(ctx, "/"+clusterName)
	}
	if err != nil {
		logf.FromContext(ctx).V(1).Info("falling back to local client for DNSZone lookup",
			"cluster", clusterName, "error", err)
		return m.Client
	}
	return cl.GetClient()
}

// SetupDNSRecordSetWebhook registers the mutating webhook with the manager.
func SetupDNSRecordSetWebhook(mgr mcmanager.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr.GetLocalManager(), &dnsv1alpha1.DNSRecordSet{}).
		WithDefaulter(&DNSRecordSetMutator{
			Manager: mgr,
			Client:  mgr.GetLocalManager().GetClient(),
		}).
		Complete()
}
