// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	pdnsclient "go.miloapis.com/dns-operator/internal/pdns"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const dnsZoneTSIGKeyPowerDNSFinalizer = "dns.networking.miloapis.com/finalize-dnszonetsigkey-powerdns"

// DNSZoneTSIGKeyPDNS is the subset of the PowerDNS client used by the DNSZoneTSIGKey controller.
type DNSZoneTSIGKeyPDNS interface {
	EnsureTSIGKey(ctx context.Context, name, algorithm, keyMaterial string) (pdnsclient.TSIGKey, error)
	DeleteTSIGKey(ctx context.Context, id string) error
}

// DNSZoneTSIGKeyPowerDNSReconciler programs TSIG keys into PowerDNS.
type DNSZoneTSIGKeyPowerDNSReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// PDNS is optional; when nil, SetupWithManager constructs one from env via pdnsclient.NewFromEnv().
	PDNS DNSZoneTSIGKeyPDNS
}

func qualifyTSIGKeyName(keyName, zoneDomain string) string {
	// PowerDNS TSIG keys are stored by (DNS) name. Ensure we always send an FQDN
	// for the key name by appending the zone domain when keyName is not already
	// zone-qualified.
	//
	// Trailing dots are optional; preserve whether the user supplied one.
	keyName = strings.TrimSpace(keyName)
	zoneDomain = strings.TrimSpace(zoneDomain)
	if keyName == "" || zoneDomain == "" {
		return keyName
	}

	hasTrailingDot := strings.HasSuffix(keyName, ".")
	keyNoDot := strings.TrimSuffix(keyName, ".")
	zoneNoDot := strings.TrimSuffix(zoneDomain, ".")

	lKey := strings.ToLower(keyNoDot)
	lZone := strings.ToLower(zoneNoDot)

	// Already qualified for this zone (or equals zone itself).
	if lKey == lZone || strings.HasSuffix(lKey, "."+lZone) {
		if hasTrailingDot {
			return keyNoDot + "."
		}
		return keyNoDot
	}

	qualified := keyNoDot + "." + zoneNoDot
	if hasTrailingDot {
		return qualified + "."
	}
	return qualified
}

// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszonetsigkeys,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszonetsigkeys/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszonetsigkeys/finalizers,verbs=update
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones,verbs=get;list;watch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszoneclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *DNSZoneTSIGKeyPowerDNSReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("namespace", req.Namespace, "name", req.Name)
	logger.Info("dnszonetsigkey powerdns reconcile start")

	var tk dnsv1alpha1.DNSZoneTSIGKey
	if err := r.Get(ctx, req.NamespacedName, &tk); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion path: delete from PDNS, then drop finalizer.
	if !tk.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&tk, dnsZoneTSIGKeyPowerDNSFinalizer) {
			pdnsCli := r.PDNS
			if pdnsCli == nil {
				return ctrl.Result{}, fmt.Errorf("pdns client is nil (SetupWithManager not called?)")
			}
			// Best-effort cleanup by ID.
			if tk.Status.TSIGKeyName != "" {
				if err := pdnsCli.DeleteTSIGKey(ctx, tk.Status.TSIGKeyName); err != nil {
					logger.Error(err, "failed to delete PDNS TSIG key by id; will retry", "id", tk.Status.TSIGKeyName)
					return ctrl.Result{}, err
				}
			} else {
				// If we don't have a provider ID, we can't safely delete an external key.
				// This commonly means the key was never successfully programmed.
				logger.Info("skipping PDNS TSIG key delete (missing status.tsigKeyName)")
			}

			base := tk.DeepCopy()
			controllerutil.RemoveFinalizer(&tk, dnsZoneTSIGKeyPowerDNSFinalizer)
			if err := r.Patch(ctx, &tk, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer while active.
	if !controllerutil.ContainsFinalizer(&tk, dnsZoneTSIGKeyPowerDNSFinalizer) {
		base := tk.DeepCopy()
		controllerutil.AddFinalizer(&tk, dnsZoneTSIGKeyPowerDNSFinalizer)
		if err := r.Patch(ctx, &tk, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Resolve zone + class and ensure this DNSZoneTSIGKey is for a PowerDNS zone.
	zone, ok, err := r.resolveZone(ctx, &tk)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ok {
		// dependency missing or wrong controller; status already updated
		return ctrl.Result{}, nil
	}
	_, ok, err = r.resolveZoneClass(ctx, &tk, zone)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ok {
		// dependency missing or wrong controller; status already updated
		return ctrl.Result{}, nil
	}

	// Ensure the DNSZone is an owner of this DNSZoneTSIGKey so GC cascades on zone deletion.
	if !metav1.IsControlledBy(&tk, zone) {
		base := tk.DeepCopy()
		if err := controllerutil.SetControllerReference(zone, &tk, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Patch(ctx, &tk, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Determine effective algorithm.
	alg := tk.Spec.Algorithm
	if alg == "" {
		alg = dnsv1alpha1.TSIGAlgorithmHMACMD5
	}

	// Resolve key material from Secret (BYO) or generated secret.
	secretName, keyMaterial, ok, err := r.resolveKeyMaterial(ctx, &tk)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Update status.secretName if needed (even when the Secret is not present yet).
	// This ensures Secret watch events can enqueue the owning DNSZoneTSIGKey.
	if secretName != "" && tk.Status.SecretName != secretName {
		base := tk.DeepCopy()
		tk.Status.SecretName = secretName
		if err := r.Status().Patch(ctx, &tk, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		// re-fetch not required; continue
	}
	if !ok {
		// invalid secret schema etc; Accepted updated
		return ctrl.Result{}, nil
	}

	if err := r.setAcceptedCondition(ctx, &tk, metav1.ConditionTrue, ReasonAccepted, "Accepted for zone"); err != nil {
		return ctrl.Result{}, err
	}

	ensureName := qualifyTSIGKeyName(tk.Spec.KeyName, zone.Spec.DomainName)
	created, pdnsErr := r.PDNS.EnsureTSIGKey(ctx, ensureName, string(alg), keyMaterial)
	if pdnsErr != nil {
		_ = r.setProgrammedCondition(ctx, &tk, metav1.ConditionFalse, ReasonPDNSError, pdnsErr.Error())
		return ctrl.Result{}, pdnsErr
	}

	// Persist provider identifier (PowerDNS ID). This is 1:1 with the TSIG key name in PDNS,
	// but we expose it under status.tsigKeyName to align with the upstream "wire name".
	id := created.ID
	if id != "" && tk.Status.TSIGKeyName != id {
		base := tk.DeepCopy()
		tk.Status.TSIGKeyName = id
		if err := r.Status().Patch(ctx, &tk, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.setProgrammedCondition(ctx, &tk, metav1.ConditionTrue, ReasonProgrammed, "TSIG key programmed"); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("dnszonetsigkey powerdns reconcile complete")
	return ctrl.Result{}, nil
}

func (r *DNSZoneTSIGKeyPowerDNSReconciler) resolveZone(ctx context.Context, tk *dnsv1alpha1.DNSZoneTSIGKey) (*dnsv1alpha1.DNSZone, bool, error) {
	// Zone lookup
	var zone dnsv1alpha1.DNSZone
	if err := r.Get(ctx, client.ObjectKey{Namespace: tk.Namespace, Name: tk.Spec.DNSZoneRef.Name}, &zone); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.setAcceptedCondition(ctx, tk, metav1.ConditionFalse, ReasonPending,
				fmt.Sprintf("waiting for DNSZone %q", tk.Spec.DNSZoneRef.Name)); err != nil {
				return nil, false, err
			}
			return nil, false, nil
		}
		return nil, false, err
	}
	if !zone.DeletionTimestamp.IsZero() {
		if err := r.setAcceptedCondition(ctx, tk, metav1.ConditionFalse, ReasonPending,
			fmt.Sprintf("DNSZone %q is deleting", zone.Name)); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	return &zone, true, nil
}

func (r *DNSZoneTSIGKeyPowerDNSReconciler) resolveZoneClass(ctx context.Context, tk *dnsv1alpha1.DNSZoneTSIGKey, zone *dnsv1alpha1.DNSZone) (*dnsv1alpha1.DNSZoneClass, bool, error) {
	if zone.Spec.DNSZoneClassName == "" {
		if err := r.setAcceptedCondition(ctx, tk, metav1.ConditionFalse, ReasonPending,
			fmt.Sprintf("DNSZone %q has no class yet", zone.Name)); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}

	// Class lookup
	var zc dnsv1alpha1.DNSZoneClass
	if err := r.Get(ctx, client.ObjectKey{Name: zone.Spec.DNSZoneClassName}, &zc); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.setAcceptedCondition(ctx, tk, metav1.ConditionFalse, ReasonPending,
				fmt.Sprintf("DNSZoneClass %q not found", zone.Spec.DNSZoneClassName)); err != nil {
				return nil, false, err
			}
			return nil, false, nil
		}
		return nil, false, err
	}
	if zc.Spec.ControllerName != ControllerNamePowerDNS {
		if err := r.setAcceptedCondition(ctx, tk, metav1.ConditionFalse, ReasonPending,
			fmt.Sprintf("DNSZoneClass controller %q is not %q", zc.Spec.ControllerName, ControllerNamePowerDNS)); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	return &zc, true, nil
}

func (r *DNSZoneTSIGKeyPowerDNSReconciler) resolveKeyMaterial(ctx context.Context, tk *dnsv1alpha1.DNSZoneTSIGKey) (secretName string, keyMaterial string, ok bool, err error) {
	// BYO secret: validate schema and do not mutate.
	if tk.Spec.SecretRef != nil && tk.Spec.SecretRef.Name != "" {
		var s corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: tk.Namespace, Name: tk.Spec.SecretRef.Name}, &s); err != nil {
			if apierrors.IsNotFound(err) {
				_ = r.setAcceptedCondition(ctx, tk, metav1.ConditionFalse, ReasonPending, fmt.Sprintf("waiting for Secret %q", tk.Spec.SecretRef.Name))
				return tk.Spec.SecretRef.Name, "", false, nil
			}
			return "", "", false, err
		}
		secB := s.Data["secret"]
		if len(secB) == 0 {
			_ = r.setAcceptedCondition(ctx, tk, metav1.ConditionFalse, ReasonInvalidSecret, "secret.data.secret must be non-empty")
			return s.Name, "", false, nil
		}
		// PDNS expects base64 key material. Secret.data.secret is already stored base64-encoded
		// by Kubernetes at the API layer, so the bytes we get here are the original raw secret bytes.
		return s.Name, base64.StdEncoding.EncodeToString(secB), true, nil
	}

	// Generated secret mode: the replicator is responsible for creating and replicating this secret.
	// The downstream controller only consumes and validates it.
	secretName = tk.Name
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: tk.Namespace, Name: secretName}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			_ = r.setAcceptedCondition(ctx, tk, metav1.ConditionFalse, ReasonPending, fmt.Sprintf("waiting for Secret %q", secretName))
			return secretName, "", false, nil
		}
		return "", "", false, err
	}
	secB := secret.Data["secret"]
	if len(secB) == 0 {
		_ = r.setAcceptedCondition(ctx, tk, metav1.ConditionFalse, ReasonInvalidSecret, "secret.data.secret must be non-empty")
		return secretName, "", false, nil
	}
	return secretName, base64.StdEncoding.EncodeToString(secB), true, nil
}

func (r *DNSZoneTSIGKeyPowerDNSReconciler) setAcceptedCondition(ctx context.Context, tk *dnsv1alpha1.DNSZoneTSIGKey, status metav1.ConditionStatus, reason, message string) error {
	base := tk.DeepCopy()
	cond := metav1.Condition{
		Type:               CondAccepted,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: tk.Generation,
		LastTransitionTime: metav1.NewTime(time.Now()),
	}
	if !apimeta.SetStatusCondition(&tk.Status.Conditions, cond) {
		return nil
	}
	return r.Status().Patch(ctx, tk, client.MergeFrom(base))
}

func (r *DNSZoneTSIGKeyPowerDNSReconciler) setProgrammedCondition(ctx context.Context, tk *dnsv1alpha1.DNSZoneTSIGKey, status metav1.ConditionStatus, reason, message string) error {
	base := tk.DeepCopy()
	cond := metav1.Condition{
		Type:               CondProgrammed,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: tk.Generation,
		LastTransitionTime: metav1.NewTime(time.Now()),
	}
	if !apimeta.SetStatusCondition(&tk.Status.Conditions, cond) {
		return nil
	}
	return r.Status().Patch(ctx, tk, client.MergeFrom(base))
}

func (r *DNSZoneTSIGKeyPowerDNSReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Initialize PDNS client once at setup-time (unless injected, e.g. tests).
	// This fails fast on bad env/config rather than failing on the first reconcile.
	if r.PDNS == nil {
		cli, err := pdnsclient.NewFromEnv()
		if err != nil {
			return fmt.Errorf("pdns client: %w", err)
		}
		r.PDNS = cli
	}

	// index DNSZoneTSIGKey by dnsZoneRef for quick fan-out from a DNSZone event
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&dnsv1alpha1.DNSZoneTSIGKey{}, "spec.DNSZoneRef.Name",
		func(obj client.Object) []string {
			tk := obj.(*dnsv1alpha1.DNSZoneTSIGKey)
			return []string{tk.Spec.DNSZoneRef.Name}
		},
	); err != nil {
		return err
	}

	// Index DNSZoneTSIGKey by the effective secret name stored in status.secretName.
	// This is what the controller actually consumes (BYO secretRef or generated).
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&dnsv1alpha1.DNSZoneTSIGKey{}, "status.secretName",
		func(obj client.Object) []string {
			tk := obj.(*dnsv1alpha1.DNSZoneTSIGKey)
			if tk.Status.SecretName != "" {
				return []string{tk.Status.SecretName}
			}
			return nil
		},
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&dnsv1alpha1.DNSZoneTSIGKey{}).
		// When a DNSZone changes, enqueue its DNSZoneTSIGKeys.
		Watches(
			&dnsv1alpha1.DNSZone{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
				zone := obj.(*dnsv1alpha1.DNSZone)
				var tks dnsv1alpha1.DNSZoneTSIGKeyList
				_ = mgr.GetClient().List(ctx, &tks, client.InNamespace(zone.Namespace), client.MatchingFields{"spec.DNSZoneRef.Name": zone.Name})
				out := make([]ctrl.Request, 0, len(tks.Items))
				for i := range tks.Items {
					out = append(out, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&tks.Items[i])})
				}
				return out
			}),
		).
		// When a BYO secret changes, enqueue the DNSZoneTSIGKeys referencing it.
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
				sec := obj.(*corev1.Secret)
				var tks dnsv1alpha1.DNSZoneTSIGKeyList
				_ = mgr.GetClient().List(ctx, &tks, client.InNamespace(sec.Namespace), client.MatchingFields{"status.secretName": sec.Name})
				out := make([]ctrl.Request, 0, len(tks.Items))
				for i := range tks.Items {
					out = append(out, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&tks.Items[i])})
				}
				return out
			}),
		).
		Named("dnszonetsigkey-powerdns").
		Complete(r)
}
