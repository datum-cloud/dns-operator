// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	crwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

const (
	mutatePath   = "/mutate-dns-networking-miloapis-com-v1alpha1-dnsrecordset"
	validatePath = "/validate-dns-networking-miloapis-com-v1alpha1-dnsrecordset"
)

func TestSetupDNSRecordSetWebhook_IsServed(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if dir := envTestBinaryDir(t); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v (run 'make setup-envtest' or set KUBEBUILDER_ASSETS)", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})

	serving := testEnv.WebhookInstallOptions
	server := NewClusterAwareWebhookServer(crwebhook.NewServer(crwebhook.Options{
		Host:    serving.LocalServingHost,
		Port:    serving.LocalServingPort,
		CertDir: serving.LocalServingCertDir,
	}))

	mgr, err := mcmanager.New(cfg, nil, ctrl.Options{
		Scheme:                 scheme,
		WebhookServer:          server,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("creating multicluster manager: %v", err)
	}
	if err := SetupDNSRecordSetWebhook(mgr); err != nil {
		t.Fatalf("SetupDNSRecordSetWebhook: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- mgr.Start(ctx) }()

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "my-zone", Namespace: "default"},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.com"},
	}
	if err := k8sClient.Create(ctx, zone); err != nil {
		t.Fatalf("creating DNSZone: %v", err)
	}
	incumbent := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "incumbent", Namespace: "default"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "my-zone"},
			RecordType: dnsv1alpha1.RRTypeTXT,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "www", TXT: &dnsv1alpha1.TXTRecordSpec{Content: "incumbent"}},
			},
		},
	}
	if err := k8sClient.Create(ctx, incumbent); err != nil {
		t.Fatalf("creating incumbent DNSRecordSet: %v", err)
	}
	awaitCachedFixtures(ctx, t, mgr.GetLocalManager().GetClient())

	newcomer := incumbent.DeepCopy()
	newcomer.ObjectMeta = metav1.ObjectMeta{Name: "newcomer", Namespace: "default"}
	newcomer.Spec.Records[0].TXT = &dnsv1alpha1.TXTRecordSpec{Content: "newcomer"}

	httpClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec
	}
	endpoint := func(path string) string {
		return fmt.Sprintf("https://%s:%d%s", serving.LocalServingHost, serving.LocalServingPort, path)
	}

	t.Run("mutating path answers", func(t *testing.T) {
		resp := postAdmissionReview(ctx, t, httpClient, endpoint(mutatePath), admissionv1.Create, newcomer, nil)
		if !resp.Allowed {
			t.Fatalf("mutating webhook denied the request: %v", resp.Result)
		}
		if len(resp.Patch) == 0 {
			t.Error("mutating webhook returned no patch; display annotations were not stamped")
		}
	})

	t.Run("validating path refuses a claimed owner name", func(t *testing.T) {
		resp := postAdmissionReview(ctx, t, httpClient, endpoint(validatePath), admissionv1.Create, newcomer, nil)
		if resp.Allowed {
			t.Fatal("validating webhook accepted a claimed owner name")
		}
		if resp.Result == nil || resp.Result.Reason != metav1.StatusReasonInvalid {
			t.Fatalf("refusal result = %v, want reason Invalid", resp.Result)
		}
		for _, want := range []string{"www.example.com.", "incumbent"} {
			if !strings.Contains(resp.Result.Message, want) {
				t.Errorf("refusal %q does not mention %q", resp.Result.Message, want)
			}
		}
	})

	t.Run("validating path accepts an uncontested owner name", func(t *testing.T) {
		free := newcomer.DeepCopy()
		free.Spec.Records[0].Name = "mail"
		resp := postAdmissionReview(ctx, t, httpClient, endpoint(validatePath), admissionv1.Create, free, nil)
		if !resp.Allowed {
			t.Fatalf("validating webhook refused an uncontested name: %v", resp.Result)
		}
	})

	t.Run("validating path leaves an already conflicted set editable", func(t *testing.T) {
		conflicted := newcomer.DeepCopy()
		conflicted.Name = "conflicted"
		updated := conflicted.DeepCopy()
		updated.Spec.Records[0].TXT = &dnsv1alpha1.TXTRecordSpec{Content: "edited"}
		resp := postAdmissionReview(ctx, t, httpClient, endpoint(validatePath), admissionv1.Update, updated, conflicted)
		if !resp.Allowed {
			t.Fatalf("validating webhook wedged an already conflicted set: %v", resp.Result)
		}
	})

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("manager returned: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("manager did not shut down")
	}
}

func awaitCachedFixtures(ctx context.Context, t *testing.T, cached client.Client) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var zone dnsv1alpha1.DNSZone
		zoneErr := cached.Get(ctx, types.NamespacedName{Namespace: "default", Name: "my-zone"}, &zone)
		var rs dnsv1alpha1.DNSRecordSet
		rsErr := cached.Get(ctx, types.NamespacedName{Namespace: "default", Name: "incumbent"}, &rs)
		if zoneErr == nil && rsErr == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("manager cache never served the fixtures: zone %v, recordset %v", zoneErr, rsErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func postAdmissionReview(
	ctx context.Context,
	t *testing.T,
	httpClient *http.Client,
	url string,
	operation admissionv1.Operation,
	obj, oldObj *dnsv1alpha1.DNSRecordSet,
) *admissionv1.AdmissionResponse {
	t.Helper()

	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshalling object: %v", err)
	}
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Operation: operation,
			Namespace: obj.Namespace,
			Name:      obj.Name,
			Kind: metav1.GroupVersionKind{
				Group:   dnsv1alpha1.GroupVersion.Group,
				Version: dnsv1alpha1.GroupVersion.Version,
				Kind:    "DNSRecordSet",
			},
			Resource: metav1.GroupVersionResource{
				Group:    dnsv1alpha1.GroupVersion.Group,
				Version:  dnsv1alpha1.GroupVersion.Version,
				Resource: "dnsrecordsets",
			},
			Object: runtime.RawExtension{Raw: raw},
		},
	}
	if oldObj != nil {
		oldRaw, err := json.Marshal(oldObj)
		if err != nil {
			t.Fatalf("marshalling old object: %v", err)
		}
		review.Request.OldObject = runtime.RawExtension{Raw: oldRaw}
	}

	body, err := json.Marshal(review)
	if err != nil {
		t.Fatalf("marshalling AdmissionReview: %v", err)
	}

	httpResp, err := postUntilServing(ctx, httpClient, url, body)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: status %d, want 200; the path is not served", url, httpResp.StatusCode)
	}

	var out admissionv1.AdmissionReview
	if err := json.NewDecoder(httpResp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding AdmissionReview from %s: %v", url, err)
	}
	if out.Response == nil {
		t.Fatalf("POST %s returned no AdmissionResponse", url)
	}
	return out.Response
}

func postUntilServing(ctx context.Context, httpClient *http.Client, url string, body []byte) (*http.Response, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err == nil {
			return resp, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func envTestBinaryDir(t *testing.T) string {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		return ""
	}
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(base, entry.Name())
		}
	}
	return ""
}
