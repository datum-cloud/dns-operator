// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"crypto/rand"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
	mcsource "sigs.k8s.io/multicluster-runtime/pkg/source"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/downstreamclient"
	corev1 "k8s.io/api/core/v1"
)

const dnsZoneTSIGKeyFinalizer = "dns.networking.miloapis.com/finalize-dnszonetsigkey"

// DNSZoneTSIGKeyReplicator mirrors DNSZoneTSIGKey resources into the downstream cluster and reflects downstream status back upstream.
type DNSZoneTSIGKeyReplicator struct {
	DownstreamClient client.Client

	mgr mcmanager.Manager
}

// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszonetsigkeys,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszonetsigkeys/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszonetsigkeys/finalizers,verbs=update
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones,verbs=get;list;watch

func (r *DNSZoneTSIGKeyReplicator) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx).WithValues("cluster", req.ClusterName, "namespace", req.Namespace, "name", req.Name)
	ctx = log.IntoContext(ctx, lg)
	lg.Info("reconcile start")

	upstreamCluster, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	var upstream dnsv1alpha1.DNSZoneTSIGKey
	if err := upstreamCluster.GetClient().Get(ctx, req.NamespacedName, &upstream); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	strategy := downstreamclient.NewMappedNamespaceResourceStrategy(req.ClusterName, upstreamCluster.GetClient(), r.DownstreamClient)

	// Ensure upstream finalizer (non-deletion path)
	if upstream.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(&upstream, dnsZoneTSIGKeyFinalizer) {
		base := upstream.DeepCopy()
		controllerutil.AddFinalizer(&upstream, dnsZoneTSIGKeyFinalizer)
		if err := upstreamCluster.GetClient().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Deletion path: delete downstream shadow first then remove finalizer.
	if !upstream.DeletionTimestamp.IsZero() {
		done, err := r.handleDeletion(ctx, upstreamCluster.GetClient(), strategy, &upstream)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, nil
	}

	// Gate on referenced DNSZone early and update status when missing
	var zone dnsv1alpha1.DNSZone
	if err := upstreamCluster.GetClient().Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: upstream.Spec.DNSZoneRef.Name}, &zone); err != nil {
		if apierrors.IsNotFound(err) {
			zoneMsg := fmt.Sprintf("DNSZone %q not found", upstream.Spec.DNSZoneRef.Name)
			if apimeta.SetStatusCondition(&upstream.Status.Conditions, metav1.Condition{
				Type:               CondAccepted,
				Status:             metav1.ConditionFalse,
				Reason:             ReasonPending,
				Message:            zoneMsg,
				ObservedGeneration: upstream.Generation,
				LastTransitionTime: metav1.Now(),
			}) {
				base := upstream.DeepCopy()
				if err := upstreamCluster.GetClient().Status().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// If the zone is being deleted, do not program downstream key
	if !zone.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Ensure OwnerReference to upstream DNSZone (same ns)
	if !metav1.IsControlledBy(&upstream, &zone) {
		base := upstream.DeepCopy()
		if err := controllerutil.SetControllerReference(&zone, &upstream, upstreamCluster.GetScheme()); err != nil {
			return ctrl.Result{}, err
		}
		if err := upstreamCluster.GetClient().Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Ensure downstream shadow DNSZoneTSIGKey mirrors upstream spec.
	if _, err := r.ensureDownstreamDNSZoneTSIGKey(ctx, req.ClusterName, strategy, &upstream); err != nil {
		return ctrl.Result{}, err
	}

	// Ensure Secret is present upstream and replicated to downstream so PowerDNS can consume it.
	if err := r.ensureSecretReplication(ctx, req.ClusterName, upstreamCluster.GetClient(), strategy, &upstream); err != nil {
		return ctrl.Result{}, err
	}

	// Mirror downstream status when the shadow exists.
	md, mdErr := strategy.ObjectMetaFromUpstreamObject(ctx, &upstream)
	if mdErr != nil {
		return ctrl.Result{}, mdErr
	}
	var shadow dnsv1alpha1.DNSZoneTSIGKey
	if err := r.DownstreamClient.Get(ctx, types.NamespacedName{Namespace: md.Namespace, Name: md.Name}, &shadow); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.updateStatus(ctx, upstreamCluster.GetClient(), &upstream, shadow.Status.DeepCopy()); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *DNSZoneTSIGKeyReplicator) handleDeletion(ctx context.Context, c client.Client, strategy downstreamclient.ResourceStrategy, upstream *dnsv1alpha1.DNSZoneTSIGKey) (done bool, err error) {
	if !controllerutil.ContainsFinalizer(upstream, dnsZoneTSIGKeyFinalizer) {
		return true, nil
	}

	// Best-effort explicit deletes.
	secretName := upstream.Name
	generatedSecret := true
	if upstream.Spec.SecretRef != nil && upstream.Spec.SecretRef.Name != "" {
		generatedSecret = false
		secretName = upstream.Spec.SecretRef.Name
	}

	md, err := strategy.ObjectMetaFromUpstreamObject(ctx, upstream)
	if err != nil {
		return false, err
	}

	// Only delete the Secret in generated-secret mode. In BYO mode, multiple
	// DNSZoneTSIGKeys can reference the same Secret name, so deleting it here
	// would break other keys.
	if generatedSecret {
		var secret corev1.Secret
		secret.SetNamespace(md.Namespace)
		secret.SetName(secretName)
		if err := r.DownstreamClient.Delete(ctx, &secret); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}

	// Then delete the downstream shadow.
	var shadow dnsv1alpha1.DNSZoneTSIGKey
	shadow.SetNamespace(md.Namespace)
	shadow.SetName(md.Name)
	if err := r.DownstreamClient.Delete(ctx, &shadow); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}

	// Finally delete the anchor ConfigMap for this upstream object. This drives GC for
	// downstream artifacts (shadow + replicated Secret) that are owned via the anchor.
	if err := strategy.DeleteAnchorForObject(ctx, upstream); err != nil {
		return false, err
	}

	base := upstream.DeepCopy()
	controllerutil.RemoveFinalizer(upstream, dnsZoneTSIGKeyFinalizer)
	if err := c.Patch(ctx, upstream, client.MergeFrom(base)); err != nil {
		return false, err
	}
	return true, nil
}

func (r *DNSZoneTSIGKeyReplicator) ensureDownstreamDNSZoneTSIGKey(ctx context.Context, upstreamClusterName string, strategy downstreamclient.ResourceStrategy, upstream *dnsv1alpha1.DNSZoneTSIGKey) (controllerutil.OperationResult, error) {
	md, err := strategy.ObjectMetaFromUpstreamObject(ctx, upstream)
	if err != nil {
		return controllerutil.OperationResultNone, err
	}

	shadow := dnsv1alpha1.DNSZoneTSIGKey{}
	shadow.SetNamespace(md.Namespace)
	shadow.SetName(md.Name)

	// Ensure we create in the correct mapped namespace (and that it exists) by using the strategy client.
	dsClient := strategy.GetClient()

	res, cErr := controllerutil.CreateOrPatch(ctx, dsClient, &shadow, func() error {
		shadow.Labels = md.Labels

		if !equality.Semantic.DeepEqual(shadow.Spec, upstream.Spec) {
			shadow.Spec = upstream.Spec
		}

		// Set owner reference using the mapped-namespace strategy (anchor-based).
		// Anchor deletion is handled in the upstream deletion path (handleDeletion).
		return strategy.SetControllerReference(ctx, upstream, &shadow)
	})
	if cErr != nil {
		return res, cErr
	}
	log.FromContext(ctx).Info("ensured downstream DNSZoneTSIGKey", "operation", res, "namespace", shadow.Namespace, "name", shadow.Name)
	return res, nil
}

func (r *DNSZoneTSIGKeyReplicator) updateStatus(ctx context.Context, c client.Client, upstream *dnsv1alpha1.DNSZoneTSIGKey, downstreamStatus *dnsv1alpha1.DNSZoneTSIGKeyStatus) error {
	if downstreamStatus == nil {
		return nil
	}
	if equality.Semantic.DeepEqual(upstream.Status, *downstreamStatus) {
		return nil
	}
	base := upstream.DeepCopy()
	upstream.Status = *downstreamStatus
	return c.Status().Patch(ctx, upstream, client.MergeFrom(base))
}

func (r *DNSZoneTSIGKeyReplicator) ensureSecretReplication(ctx context.Context, upstreamClusterName string, upstreamClient client.Client, strategy downstreamclient.ResourceStrategy, upstream *dnsv1alpha1.DNSZoneTSIGKey) error {
	// Determine the source secret name.
	secretName := upstream.Name
	if upstream.Spec.SecretRef != nil && upstream.Spec.SecretRef.Name != "" {
		secretName = upstream.Spec.SecretRef.Name
	}

	// Determine algorithm (default if omitted).
	alg := upstream.Spec.Algorithm
	if alg == "" {
		alg = dnsv1alpha1.TSIGAlgorithmHMACMD5
	}

	// Ensure upstream secret exists (create only in generated-secret mode).
	var src corev1.Secret
	if err := upstreamClient.Get(ctx, client.ObjectKey{Namespace: upstream.Namespace, Name: secretName}, &src); err != nil {
		if apierrors.IsNotFound(err) {
			if upstream.Spec.SecretRef != nil && upstream.Spec.SecretRef.Name != "" {
				// BYO secret not found yet; wait.
				return nil
			}

			// Generated mode: create the secret upstream.
			src = corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: upstream.Namespace},
				Type:       corev1.SecretTypeOpaque,
				Data:       map[string][]byte{},
			}
			_, err := controllerutil.CreateOrPatch(ctx, upstreamClient, &src, func() error {
				if src.Type == "" {
					src.Type = corev1.SecretTypeOpaque
				}
				if src.Data == nil {
					src.Data = map[string][]byte{}
				}
				if len(src.Data["secret"]) == 0 {
					raw := make([]byte, tsigKeySecretLen(alg))
					if _, err := rand.Read(raw); err != nil {
						return err
					}
					// Store raw secret bytes. (PowerDNS expects base64, but that is derived at reconciliation time.)
					src.Data["secret"] = raw
				}
				return controllerutil.SetControllerReference(upstream, &src, upstreamClient.Scheme())
			})
			return err
		}
		return err
	}

	// Ensure downstream secret mirrors upstream secret data.
	md, err := strategy.ObjectMetaFromUpstreamObject(ctx, upstream)
	if err != nil {
		return err
	}
	dsClient := strategy.GetClient() // ensures downstream namespace exists on Create

	// Fetch downstream DNSZoneTSIGKey shadow for owner reference (GC in downstream).
	var shadow dnsv1alpha1.DNSZoneTSIGKey
	if err := r.DownstreamClient.Get(ctx, types.NamespacedName{Namespace: md.Namespace, Name: md.Name}, &shadow); err != nil {
		return err
	}

	dst := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: md.Namespace, Name: secretName}}
	_, err = controllerutil.CreateOrPatch(ctx, dsClient, dst, func() error {
		if dst.Type == "" {
			dst.Type = corev1.SecretTypeOpaque
		}
		if dst.Data == nil {
			dst.Data = map[string][]byte{}
		}
		// Copy secret bytes exactly.
		dst.Data["secret"] = append([]byte(nil), src.Data["secret"]...)

		// Set owner reference using the mapped-namespace strategy (anchor-based).
		// Anchor deletion is handled in the upstream deletion path (handleDeletion).
		if err := strategy.SetControllerReference(ctx, upstream, dst); err != nil {
			return err
		}

		return nil
	})
	return err
}

func (r *DNSZoneTSIGKeyReplicator) SetupWithManager(mgr mcmanager.Manager, downstreamCl cluster.Cluster) error {
	r.mgr = mgr

	b := builder.ControllerManagedBy(mgr)
	b = b.For(&dnsv1alpha1.DNSZoneTSIGKey{})

	src := mcsource.TypedKind(
		&dnsv1alpha1.DNSZoneTSIGKey{},
		downstreamclient.TypedEnqueueRequestForUpstreamOwner[*dnsv1alpha1.DNSZoneTSIGKey](&dnsv1alpha1.DNSZoneTSIGKey{}),
	)
	clusterSrc, err := src.ForCluster("", downstreamCl)
	if err != nil {
		return fmt.Errorf("failed to build downstream watch for %s: %w", dnsv1alpha1.GroupVersion.WithKind("DNSZoneTSIGKey").String(), err)
	}
	b = b.WatchesRawSource(clusterSrc)

	// Also watch downstream Secrets (generated or BYO replicated) to ensure upstream DNSZoneTSIGKey reconcile
	// happens when secret material changes (or is first created).
	secretSrc := mcsource.TypedKind(
		&corev1.Secret{},
		downstreamclient.TypedEnqueueRequestForUpstreamOwner[*corev1.Secret](&dnsv1alpha1.DNSZoneTSIGKey{}),
	)
	secretClusterSrc, err := secretSrc.ForCluster("", downstreamCl)
	if err != nil {
		return fmt.Errorf("failed to build downstream watch for %s: %w", corev1.SchemeGroupVersion.WithKind("Secret").String(), err)
	}
	b = b.WatchesRawSource(secretClusterSrc)

	return b.Named("dnszonetsigkey-replicator").Complete(r)
}

func tsigKeySecretLen(alg dnsv1alpha1.TSIGAlgorithm) int {
	// Align with HMAC guidance: key length == hash output size.
	// (RFC 2845 for TSIG; RFC 4635 adds the SHA2-based TSIG algorithms.)
	switch alg {
	case dnsv1alpha1.TSIGAlgorithmHMACMD5:
		return 16
	case dnsv1alpha1.TSIGAlgorithmHMACSHA1:
		return 20
	case dnsv1alpha1.TSIGAlgorithmHMACSHA224:
		return 28
	case dnsv1alpha1.TSIGAlgorithmHMACSHA256:
		return 32
	case dnsv1alpha1.TSIGAlgorithmHMACSHA384:
		return 48
	case dnsv1alpha1.TSIGAlgorithmHMACSHA512:
		return 64
	default:
		// Unknown/empty algorithm: keep existing behavior (safe default).
		return 32
	}
}
