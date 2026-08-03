// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/display"
)

func TestDNSRecordSetMutator_Default(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "my-zone", Namespace: "default"},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.com"},
	}

	tests := []struct {
		name           string
		objs           []runtime.Object
		rs             *dnsv1alpha1.DNSRecordSet
		wantName       string
		wantValue      string
		wantAnnotation bool
	}{
		{
			name: "zone present stamps FQDN and value",
			objs: []runtime.Object{zone},
			rs: &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{Name: "www", Namespace: "default"},
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					DNSZoneRef: corev1.LocalObjectReference{Name: "my-zone"},
					RecordType: dnsv1alpha1.RRTypeA,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}}},
				},
			},
			wantName:       "www.example.com",
			wantValue:      "192.0.2.10",
			wantAnnotation: true,
		},
		{
			name: "apex record uses zone domain",
			objs: []runtime.Object{zone},
			rs: &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{Name: "apex", Namespace: "default"},
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					DNSZoneRef: corev1.LocalObjectReference{Name: "my-zone"},
					RecordType: dnsv1alpha1.RRTypeA,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.1"}}},
				},
			},
			wantName:       "example.com",
			wantValue:      "192.0.2.1",
			wantAnnotation: true,
		},
		{
			name: "dmarc TXT",
			objs: []runtime.Object{zone},
			rs: &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{Name: "dmarc", Namespace: "default"},
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					DNSZoneRef: corev1.LocalObjectReference{Name: "my-zone"},
					RecordType: dnsv1alpha1.RRTypeTXT,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "_dmarc", TXT: &dnsv1alpha1.TXTRecordSpec{Content: "v=DMARC1; p=none"}}},
				},
			},
			wantName:       "_dmarc.example.com",
			wantValue:      "\"v=DMARC1; p=none\"",
			wantAnnotation: true,
		},
		{
			name: "missing zone leaves annotations unset",
			objs: nil,
			rs: &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{Name: "www", Namespace: "default"},
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					DNSZoneRef: corev1.LocalObjectReference{Name: "missing"},
					RecordType: dnsv1alpha1.RRTypeA,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}}},
				},
			},
			wantAnnotation: false,
		},
		{
			name: "update refreshes display-value",
			objs: []runtime.Object{zone},
			rs: &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "www",
					Namespace: "default",
					Annotations: map[string]string{
						display.AnnotationDisplayName:  "www.example.com",
						display.AnnotationDisplayValue: "192.0.2.10",
					},
				},
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					DNSZoneRef: corev1.LocalObjectReference{Name: "my-zone"},
					RecordType: dnsv1alpha1.RRTypeA,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.99"}}},
				},
			},
			wantName:       "www.example.com",
			wantValue:      "192.0.2.99",
			wantAnnotation: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tt.objs...).Build()
			m := &DNSRecordSetMutator{Client: cl}
			rs := tt.rs.DeepCopy()
			if err := m.Default(context.Background(), rs); err != nil {
				t.Fatalf("Default: %v", err)
			}
			if !tt.wantAnnotation {
				if rs.Annotations != nil {
					if _, ok := rs.Annotations[display.AnnotationDisplayName]; ok {
						t.Fatalf("unexpected display-name annotation: %v", rs.Annotations)
					}
				}
				return
			}
			if got := rs.Annotations[display.AnnotationDisplayName]; got != tt.wantName {
				t.Errorf("display-name = %q, want %q", got, tt.wantName)
			}
			if got := rs.Annotations[display.AnnotationDisplayValue]; got != tt.wantValue {
				t.Errorf("display-value = %q, want %q", got, tt.wantValue)
			}
		})
	}
}

func TestDNSRecordSetMutator_Default_activityDiff(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "my-zone", Namespace: "default"},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "dodik.me"},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(zone).Build()
	m := &DNSRecordSetMutator{Client: cl}

	oldRS := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a", Namespace: "default"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "my-zone"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.168.1.1"}}},
		},
	}
	oldRaw, err := json.Marshal(oldRS)
	if err != nil {
		t.Fatalf("marshal old: %v", err)
	}

	newRS := oldRS.DeepCopy()
	newRS.Spec.Records = append(newRS.Spec.Records, dnsv1alpha1.RecordEntry{
		Name: "app", A: &dnsv1alpha1.ARecordSpec{Content: "192.168.1.1"},
	})

	ctx := admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			OldObject: runtime.RawExtension{Raw: oldRaw},
		},
	})
	if err := m.Default(ctx, newRS); err != nil {
		t.Fatalf("Default: %v", err)
	}
	if got := newRS.Annotations[display.AnnotationActivityChange]; got != display.ActivityChangeAdded {
		t.Errorf("activity-change = %q, want %q", got, display.ActivityChangeAdded)
	}
	if got := newRS.Annotations[display.AnnotationActivityName]; got != "app.dodik.me" {
		t.Errorf("activity-name = %q, want app.dodik.me", got)
	}
	if got := newRS.Annotations[display.AnnotationDisplayName]; got != "www.dodik.me, app.dodik.me" {
		t.Errorf("display-name = %q, want joined FQDNs", got)
	}

	// Create (no OldObject) clears activity annotations.
	createRS := oldRS.DeepCopy()
	createRS.Annotations = map[string]string{
		display.AnnotationActivityChange: display.ActivityChangeAdded,
		display.AnnotationActivityName:   "stale.dodik.me",
	}
	if err := m.Default(context.Background(), createRS); err != nil {
		t.Fatalf("Default create: %v", err)
	}
	if _, ok := createRS.Annotations[display.AnnotationActivityChange]; ok {
		t.Fatalf("create should clear activity annotations, got %v", createRS.Annotations)
	}
}

func TestDNSRecordSetMutator_Default_projectClusterClient(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "my-zone", Namespace: "default"},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "project.example.com"},
	}
	projectClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(zone).Build()
	localClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "www", Namespace: "default"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "my-zone"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}}},
		},
	}

	t.Run("bare project name retries with slash prefix", func(t *testing.T) {
		t.Parallel()
		mgr := &fakeMCManager{
			clusters: map[string]cluster.Cluster{
				"/proj-1": &fakeCluster{client: projectClient},
			},
		}
		m := &DNSRecordSetMutator{Manager: mgr, Client: localClient}
		ctx := mccontext.WithCluster(context.Background(), "proj-1")
		got := rs.DeepCopy()
		if err := m.Default(ctx, got); err != nil {
			t.Fatalf("Default: %v", err)
		}
		if want := "www.project.example.com"; got.Annotations[display.AnnotationDisplayName] != want {
			t.Errorf("display-name = %q, want %q", got.Annotations[display.AnnotationDisplayName], want)
		}
		if want := "192.0.2.10"; got.Annotations[display.AnnotationDisplayValue] != want {
			t.Errorf("display-value = %q, want %q", got.Annotations[display.AnnotationDisplayValue], want)
		}
	})

	t.Run("getcluster failure falls back to local without failing admission", func(t *testing.T) {
		t.Parallel()
		mgr := &fakeMCManager{err: fmt.Errorf("cluster not engaged")}
		m := &DNSRecordSetMutator{Manager: mgr, Client: localClient}
		ctx := mccontext.WithCluster(context.Background(), "proj-1")
		got := rs.DeepCopy()
		if err := m.Default(ctx, got); err != nil {
			t.Fatalf("Default: %v", err)
		}
		if got.Annotations != nil {
			if _, ok := got.Annotations[display.AnnotationDisplayName]; ok {
				t.Fatalf("unexpected display-name annotation: %v", got.Annotations)
			}
		}
	})
}

func TestClusterNameFromExtra(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		extra map[string]authv1.ExtraValue
		want  string
	}{
		{
			name: "project parent",
			extra: map[string]authv1.ExtraValue{
				ParentTypeExtraKey: {"Project"},
				ParentNameExtraKey: {"my-project"},
			},
			want: "my-project",
		},
		{
			name: "organization parent ignored",
			extra: map[string]authv1.ExtraValue{
				ParentTypeExtraKey: {"Organization"},
				ParentNameExtraKey: {"my-org"},
			},
			want: "",
		},
		{
			name:  "empty extra",
			extra: nil,
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := clusterNameFromExtra(tt.extra); got != tt.want {
				t.Errorf("clusterNameFromExtra = %q, want %q", got, tt.want)
			}
		})
	}
}

// fakeMCManager implements only GetCluster for mutator tests.
type fakeMCManager struct {
	mcmanager.Manager
	clusters map[string]cluster.Cluster
	err      error
}

func (f *fakeMCManager) GetCluster(_ context.Context, name string) (cluster.Cluster, error) {
	if f.err != nil {
		return nil, f.err
	}
	if cl, ok := f.clusters[name]; ok {
		return cl, nil
	}
	return nil, fmt.Errorf("cluster %q not found", name)
}

type fakeCluster struct {
	cluster.Cluster
	client client.Client
}

func (f *fakeCluster) GetClient() client.Client {
	return f.client
}
