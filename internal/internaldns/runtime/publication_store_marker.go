// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	publicationStoreMarker     = "internal-dns-publication-store"
	publicationStoreKubernetes = "kubernetes"
	publicationStoreJetStream  = "jetstream"
)

// claimPublicationStore refuses to start a control plane whose publication
// store differs from the one the platform records as active. Leases live in
// the store, so replicas on different stores would each hold every lease and
// publish as two writers. Switching stores means stopping every replica and
// changing the marker first.
func claimPublicationStore(ctx context.Context, api client.Client, namespace, store string) error {
	var marker corev1.ConfigMap
	key := client.ObjectKey{Namespace: namespace, Name: publicationStoreMarker}
	err := api.Get(ctx, key, &marker)
	if apierrors.IsNotFound(err) {
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
	if active := marker.Data["store"]; active != store {
		return fmt.Errorf("publication store %q is active in ConfigMap %s/%s but this control plane is configured for %q; stop every control-plane replica and set data.store to %q before switching", active, namespace, publicationStoreMarker, store, store)
	}
	return nil
}
