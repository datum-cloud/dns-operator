package controller_test

import (
	"context"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/controller"
	pdnsclient "go.miloapis.com/dns-operator/internal/pdns"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeDNSZoneTSIGPDNS struct {
	ensureCalls []struct {
		Name      string
		Algorithm string
		Key       string
	}
	deleteByIDCalls []string

	ensureResp pdnsclient.TSIGKey
	ensureErr  error
}

func (f *fakeDNSZoneTSIGPDNS) EnsureTSIGKey(_ context.Context, name, algorithm, keyMaterial string) (pdnsclient.TSIGKey, error) {
	f.ensureCalls = append(f.ensureCalls, struct {
		Name      string
		Algorithm string
		Key       string
	}{name, algorithm, keyMaterial})
	return f.ensureResp, f.ensureErr
}
func (f *fakeDNSZoneTSIGPDNS) DeleteTSIGKey(_ context.Context, id string) error {
	f.deleteByIDCalls = append(f.deleteByIDCalls, id)
	return nil
}

func newDNSOnlySchemeTSIG(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add dns scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return s
}

func TestDNSZoneTSIGKeyPowerDNS_ByoSecret_ValidatesAndPrograms(t *testing.T) {
	t.Parallel()

	scheme := newDNSOnlySchemeTSIG(t)
	zone, zc := newZoneAndClass("example-com")

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "byo", Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"secret": []byte("supersecret"),
		},
	}

	tk := &dnsv1alpha1.DNSZoneTSIGKey{
		ObjectMeta: metav1.ObjectMeta{Name: "xfr", Namespace: ns},
		Spec: dnsv1alpha1.DNSZoneTSIGKeySpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			KeyName:    "xfr",
			Algorithm:  dnsv1alpha1.TSIGAlgorithmHMACSHA256,
			SecretRef:  &corev1.LocalObjectReference{Name: secret.Name},
		},
	}

	wantPDNSName := "xfr.example.com"
	pdns := &fakeDNSZoneTSIGPDNS{ensureResp: pdnsclient.TSIGKey{ID: wantPDNSName, Name: wantPDNSName, Algorithm: "hmac-sha256"}}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSZoneTSIGKey{}).
		WithObjects(zone, zc, secret, tk).
		Build()

	r := &controller.DNSZoneTSIGKeyPowerDNSReconciler{Client: c, Scheme: scheme, PDNS: pdns}
	// Reconcile is multi-step (finalizer, ownerrefs, etc). Run a few times to converge.
	for i := 0; i < 5; i++ {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tk)})
		if err != nil {
			t.Fatalf("reconcile error: %v", err)
		}
	}

	var got dnsv1alpha1.DNSZoneTSIGKey
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(tk), &got); err != nil {
		t.Fatalf("get: %v", err)
	}

	if cond := apimeta.FindStatusCondition(got.Status.Conditions, controller.CondAccepted); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Accepted not true: %#v", got.Status.Conditions)
	}
	if cond := apimeta.FindStatusCondition(got.Status.Conditions, controller.CondProgrammed); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Programmed not true: %#v", got.Status.Conditions)
	}
	if got.Status.TSIGKeyName != wantPDNSName {
		t.Fatalf("expected tsigKeyName=%q, got %q", wantPDNSName, got.Status.TSIGKeyName)
	}
	if got.Status.SecretName != secret.Name {
		t.Fatalf("expected secretName=%q, got %q", secret.Name, got.Status.SecretName)
	}
	if len(pdns.ensureCalls) < 1 {
		t.Fatalf("expected at least 1 ensure call, got %d", len(pdns.ensureCalls))
	}
	if gotCall := pdns.ensureCalls[0].Name; gotCall != wantPDNSName {
		t.Fatalf("expected EnsureTSIGKey name=%q, got %q", wantPDNSName, gotCall)
	}
}

func TestDNSZoneTSIGKeyPowerDNS_ByoSecret_InvalidSchemaRejected(t *testing.T) {
	t.Parallel()

	scheme := newDNSOnlySchemeTSIG(t)
	zone, zc := newZoneAndClass("example-com")

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "byo", Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{
			// missing required secret key
		},
	}

	tk := &dnsv1alpha1.DNSZoneTSIGKey{
		ObjectMeta: metav1.ObjectMeta{Name: "xfr", Namespace: ns},
		Spec: dnsv1alpha1.DNSZoneTSIGKeySpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			KeyName:    "datum-example-com-xfr",
			Algorithm:  dnsv1alpha1.TSIGAlgorithmHMACSHA256,
			SecretRef:  &corev1.LocalObjectReference{Name: secret.Name},
		},
	}

	pdns := &fakeDNSZoneTSIGPDNS{ensureResp: pdnsclient.TSIGKey{ID: "pdns-id"}}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSZoneTSIGKey{}).
		WithObjects(zone, zc, secret, tk).
		Build()

	r := &controller.DNSZoneTSIGKeyPowerDNSReconciler{Client: c, Scheme: scheme, PDNS: pdns}
	for i := 0; i < 5; i++ {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tk)})
		if err != nil {
			t.Fatalf("reconcile error: %v", err)
		}
	}

	var got dnsv1alpha1.DNSZoneTSIGKey
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(tk), &got)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, controller.CondAccepted)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != controller.ReasonInvalidSecret {
		t.Fatalf("expected Accepted=False InvalidSecret, got %#v", got.Status.Conditions)
	}
	if len(pdns.ensureCalls) != 0 {
		t.Fatalf("expected no PDNS ensure calls when secret invalid")
	}
}

func TestDNSZoneTSIGKeyPowerDNS_GeneratesSecretAndPrograms(t *testing.T) {
	t.Parallel()

	scheme := newDNSOnlySchemeTSIG(t)
	zone, zc := newZoneAndClass("example-com")

	tk := &dnsv1alpha1.DNSZoneTSIGKey{
		ObjectMeta: metav1.ObjectMeta{Name: "xfr", Namespace: ns},
		Spec: dnsv1alpha1.DNSZoneTSIGKeySpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			KeyName:    "xfr",
			Algorithm:  dnsv1alpha1.TSIGAlgorithmHMACSHA256,
			// SecretRef omitted => generated
		},
	}

	// In generated-secret mode, the replicator is responsible for creating the Secret.
	secretName := tk.Name
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"secret": []byte("supersecret"),
		},
	}

	wantPDNSName := "xfr.example.com"
	pdns := &fakeDNSZoneTSIGPDNS{ensureResp: pdnsclient.TSIGKey{ID: wantPDNSName, Name: wantPDNSName, Algorithm: "hmac-sha256"}}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSZoneTSIGKey{}).
		WithObjects(zone, zc, tk, secret).
		Build()

	r := &controller.DNSZoneTSIGKeyPowerDNSReconciler{Client: c, Scheme: scheme, PDNS: pdns}
	for i := 0; i < 5; i++ {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tk)})
		if err != nil {
			t.Fatalf("reconcile error: %v", err)
		}
	}

	var got dnsv1alpha1.DNSZoneTSIGKey
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(tk), &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.SecretName != secretName {
		t.Fatalf("expected secretName=%q, got %q", secretName, got.Status.SecretName)
	}
	if len(pdns.ensureCalls) < 1 {
		t.Fatalf("expected PDNS ensure called")
	}
	if gotCall := pdns.ensureCalls[0].Name; gotCall != wantPDNSName {
		t.Fatalf("expected EnsureTSIGKey name=%q, got %q", wantPDNSName, gotCall)
	}
}
