package v1alpha1

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

const DownstreamRSFinalizer = "dns.networking.miloapis.com/finalize-dnsrecordset-downstream"

func SetupDownstreamDNSRecordSetWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&dnsv1alpha1.DNSRecordSet{}).
		WithDefaulter(&DownstreamDNSRecordSetDefaulter{mgr: mgr}).
		Complete()
}

// Use a distinct webhook "name" to avoid collisions with the upstream one.
// +kubebuilder:webhook:path=/mutate-dns-networking-miloapis-com-v1alpha1-dnsrecordset,mutating=true,failurePolicy=fail,sideEffects=None,groups=dns.networking.miloapis.com,resources=dnsrecordsets,verbs=create;update,versions=v1alpha1,name=mdnsrecordset-downstream-v1alpha1.kb.io,admissionReviewVersions=v1

type DownstreamDNSRecordSetDefaulter struct {
	mgr ctrl.Manager
}

var _ webhook.CustomDefaulter = &DownstreamDNSRecordSetDefaulter{}

func (d *DownstreamDNSRecordSetDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	dnsrecordset, ok := obj.(*dnsv1alpha1.DNSRecordSet)
	if !ok {
		return fmt.Errorf("expected a DNSRecordSet but got %T", obj)
	}
	// If the object is being deleted, don't add the finalizer
	if dnsrecordset.GetDeletionTimestamp() != nil {
		return nil
	}
	// Ensure downstream finalizer is set
	if !controllerutil.ContainsFinalizer(dnsrecordset, DownstreamRSFinalizer) {
		controllerutil.AddFinalizer(dnsrecordset, DownstreamRSFinalizer)
		dnsrecordsetlog.V(1).Info("added downstream finalizer", "name", dnsrecordset.GetName(), "ns", dnsrecordset.GetNamespace())
	}

	// Add ownerRef to the DNSZone (same namespace)
	if dnsrecordset.Spec.DNSZoneRef.Name != "" {
		var zone dnsv1alpha1.DNSZone
		err := d.mgr.GetClient().Get(ctx, client.ObjectKey{Namespace: dnsrecordset.Namespace, Name: dnsrecordset.Spec.DNSZoneRef.Name}, &zone)
		if err != nil {
			return err
		}
		// only add if missing
		if !hasOwnerUID(dnsrecordset.OwnerReferences, zone.UID) {
			dnsrecordset.OwnerReferences = append(dnsrecordset.OwnerReferences, metav1.OwnerReference{
				APIVersion: dnsv1alpha1.GroupVersion.String(), // "dns.networking.miloapis.com/v1alpha1"
				Kind:       zone.Kind,
				Name:       zone.Name,
				UID:        zone.UID,
			})
		}
	}
	return nil
}
