// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"fmt"

	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	publicationStoreMarker     = "internal-dns-publication-store"
	publicationStoreKubernetes = "kubernetes"
	publicationStoreJetStream  = "jetstream"
	// publicationStoreRegional is a JetStream store per region, each owned by
	// that region's control plane. It is a store of its own because a control
	// plane that publishes every region must not run beside regional ones.
	publicationStoreRegional = "jetstream-regional"
)

// claimPublicationStore refuses to start a control plane whose publication
// store differs from the one the platform records as active. Leases live in
// the store, so replicas on different stores would each hold every lease and
// publish as two writers. Switching stores means stopping every replica and
// changing the marker first.
func claimPublicationStore(ctx context.Context, api client.Client, namespace, store string, switching bool) error {
	var marker corev1.ConfigMap
	key := client.ObjectKey{Namespace: namespace, Name: publicationStoreMarker}
	err := api.Get(ctx, key, &marker)
	if apierrors.IsNotFound(err) {
		if store != publicationStoreKubernetes && !switching {
			var leases dnsv1.DNSPublicationOwnershipList
			if err := api.List(ctx, &leases, client.InNamespace(namespace), client.Limit(1)); err != nil {
				return fmt.Errorf("check for publication records in the API server: %w", err)
			}
			if len(leases.Items) > 0 {
				return fmt.Errorf("publication records exist in the API server but no ConfigMap %s/%s records the active store; stop every control-plane replica, then create it with data.store %q or start with switchPublicationStore", namespace, publicationStoreMarker, store)
			}
		}
		marker = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: namespace}, Data: map[string]string{"store": store}}
		if err := api.Create(ctx, &marker); err == nil {
			return nil
		} else if !apierrors.IsAlreadyExists(err) {
			return err
		}
		err = api.Get(ctx, key, &marker)
	}
	if err != nil {
		return fmt.Errorf("read publication store marker: %w", err)
	}
	if active := marker.Data["store"]; active != store && switching {
		base := marker.DeepCopy()
		if marker.Data == nil {
			marker.Data = map[string]string{}
		}
		marker.Data["store"] = store
		return api.Patch(ctx, &marker, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	} else if active != store {
		return fmt.Errorf("publication store %q is active in ConfigMap %s/%s but this control plane is configured for %q; stop every control-plane replica, then set data.store to %q or start with switchPublicationStore", active, namespace, publicationStoreMarker, store, store)
	}
	return nil
}
