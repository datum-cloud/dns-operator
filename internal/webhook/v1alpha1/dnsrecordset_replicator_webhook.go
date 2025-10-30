/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var dnsrecordsetlog = logf.Log.WithName("dnsrecordset-resource")

// RSFinalizer is added to every DNSRecordSet at admission so we can
// delete the downstream shadow before the upstream object is finalized.
const RSFinalizer = "dns.networking.miloapis.com/finalize-dnsrecordset"

// SetupDNSRecordSetWebhookWithManager registers the webhook for DNSRecordSet in the manager.
func SetupDNSRecordSetWebhookWithManager(mgr mcmanager.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr.GetLocalManager()).
		For(&dnsv1alpha1.DNSRecordSet{}).
		WithDefaulter(&DNSRecordSetCustomDefaulter{mgr: mgr}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-dns-networking-miloapis-com-v1alpha1-dnsrecordset,mutating=true,failurePolicy=fail,sideEffects=None,groups=dns.networking.miloapis.com,resources=dnsrecordsets,verbs=create;update,versions=v1alpha1,name=mdnsrecordset-v1alpha1.kb.io,admissionReviewVersions=v1

// DNSRecordSetCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind DNSRecordSet when those are created or updated.
type DNSRecordSetCustomDefaulter struct {
	mgr mcmanager.Manager
}

var _ webhook.CustomDefaulter = &DNSRecordSetCustomDefaulter{}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind DNSRecordSet.
func (d *DNSRecordSetCustomDefaulter) Default(ctx context.Context, obj runtime.Object) error {
	dnsrecordset, ok := obj.(*dnsv1alpha1.DNSRecordSet)
	if !ok {
		return fmt.Errorf("expected an DNSRecordSet object but got %T", obj)
	}

	// If the object is being deleted, don't add the finalizer
	if dnsrecordset.GetDeletionTimestamp() != nil {
		return nil
	}

	// Ensure the finalizer is present.
	if !controllerutil.ContainsFinalizer(dnsrecordset, RSFinalizer) {
		controllerutil.AddFinalizer(dnsrecordset, RSFinalizer)
		dnsrecordsetlog.V(1).Info("added finalizer", "name", dnsrecordset.GetName(), "finalizer", RSFinalizer)
	}

	clusterName, ok := mccontext.ClusterFrom(ctx)
	if !ok {
		return fmt.Errorf("expected a cluster name in the context")
	}

	upstreamCluster, err := d.mgr.GetCluster(ctx, clusterName)
	if err != nil {
		return err
	}

	// Add ownerRef to the upstream DNSZone (same namespace)
	if dnsrecordset.Spec.DNSZoneRef.Name != "" {
		var zone dnsv1alpha1.DNSZone
		// upstream cluster client
		err := upstreamCluster.GetClient().Get(ctx, client.ObjectKey{Namespace: dnsrecordset.Namespace, Name: dnsrecordset.Spec.DNSZoneRef.Name}, &zone)
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

func hasOwnerUID(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, r := range refs {
		if r.UID == uid {
			return true
		}
	}
	return false
}
