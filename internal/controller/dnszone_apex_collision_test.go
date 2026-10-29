package controller

import (
	"context"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCreateApexRecordSetPreservesDelegation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	delegation := &dnsv1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "child.example-ns", Namespace: "default"}, Spec: dnsv1alpha1.DNSRecordSetSpec{DNSZoneRef: corev1.LocalObjectReference{Name: "example"}, RecordType: dnsv1alpha1.RRTypeNS, Records: []dnsv1alpha1.RecordEntry{{Name: "child"}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(delegation).Build()
	desired := delegation.DeepCopy()
	desired.ResourceVersion = ""
	desired.Spec.DNSZoneRef.Name = "child.example"
	desired.Spec.Records[0].Name = "@"
	for i := 0; i < 2; i++ {
		copy := desired.DeepCopy()
		if err := createApexRecordSet(context.Background(), c, copy); err != nil {
			t.Fatal(err)
		}
	}
	var records dnsv1alpha1.DNSRecordSetList
	if err := c.List(context.Background(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records.Items) != 2 {
		t.Fatalf("got %d records, want preserved delegation and one apex", len(records.Items))
	}
	var got dnsv1alpha1.DNSRecordSet
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(delegation), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.DNSZoneRef.Name != "example" || got.Spec.Records[0].Name != "child" {
		t.Fatal("delegation overwritten")
	}
}

func TestHasApexRecordSet(t *testing.T) {
	for _, name := range []string{"child", "@"} {
		got := hasApexRecordSet([]dnsv1alpha1.DNSRecordSet{{Spec: dnsv1alpha1.DNSRecordSetSpec{Records: []dnsv1alpha1.RecordEntry{{Name: name}}}}})
		if got != (name == "@") {
			t.Fatalf("apex detection for %q = %v", name, got)
		}
	}
}
