// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
