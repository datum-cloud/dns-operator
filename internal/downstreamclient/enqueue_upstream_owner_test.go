package downstreamclient

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

func ownerAnnotations(cluster, group, kind, name, namespace string) map[string]string {
	return map[string]string{
		UpstreamOwnerClusterNameAnnotation: cluster,
		UpstreamOwnerGroupAnnotation:       group,
		UpstreamOwnerKindAnnotation:        kind,
		UpstreamOwnerNameAnnotation:        name,
		UpstreamOwnerNamespaceAnnotation:   namespace,
	}
}

func TestUpstreamOwnerMeta(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		labels      map[string]string
		wantKind    string
	}{
		{
			name:        "prefers annotations",
			annotations: ownerAnnotations("cluster-c1", "dns.example.com", "DNSRecordSet", "my-record", "default"),
			labels:      ownerAnnotations("cluster-c1", "dns.example.com", "DNSRecordSet", "stale", "stale-ns"),
			wantKind:    "DNSRecordSet",
		},
		{
			name:     "falls back to labels",
			labels:   ownerAnnotations("cluster-c1", "dns.example.com", "DNSRecordSet", "my-record", "default"),
			wantKind: "DNSRecordSet",
		},
		{
			name:     "returns nil when empty",
			wantKind: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &metav1.ObjectMeta{
				Annotations: tt.annotations,
				Labels:      tt.labels,
			}
			got := upstreamOwnerMeta(obj)
			if gotKind := got[UpstreamOwnerKindAnnotation]; gotKind != tt.wantKind {
				t.Errorf("upstreamOwnerMeta() kind = %q, want %q", gotKind, tt.wantKind)
			}
		})
	}
}

func TestGetOwnerReconcileRequest_Annotations(t *testing.T) {
	e := &enqueueRequestForOwner[client.Object]{
		groupKind: schema.GroupKind{Group: "dns.example.com", Kind: "DNSRecordSet"},
	}

	obj := &metav1.ObjectMeta{
		Annotations: ownerAnnotations(
			"cluster-_my_cluster",
			"dns.example.com",
			"DNSRecordSet",
			"v4-2ed208a11f54412a92e8a2619eb662ea-prism-staging-env-datum-net-a-a59c082e",
			"my-namespace",
		),
	}

	result := map[mcreconcile.Request]empty{}
	e.getOwnerReconcileRequest(obj, result)

	if len(result) != 1 {
		t.Fatalf("expected 1 request, got %d", len(result))
	}

	want := mcreconcile.Request{
		Request: reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "v4-2ed208a11f54412a92e8a2619eb662ea-prism-staging-env-datum-net-a-a59c082e",
				Namespace: "my-namespace",
			},
		},
		ClusterName: "/my/cluster",
	}

	for req := range result {
		if req != want {
			t.Errorf("got request %+v, want %+v", req, want)
		}
	}
}

func TestGetOwnerReconcileRequest_LabelsFallback(t *testing.T) {
	e := &enqueueRequestForOwner[client.Object]{
		groupKind: schema.GroupKind{Group: "dns.example.com", Kind: "DNSRecordSet"},
	}

	obj := &metav1.ObjectMeta{
		Labels: ownerAnnotations(
			"cluster-_my_cluster",
			"dns.example.com",
			"DNSRecordSet",
			"short-name",
			"my-namespace",
		),
	}

	result := map[mcreconcile.Request]empty{}
	e.getOwnerReconcileRequest(obj, result)

	if len(result) != 1 {
		t.Fatalf("expected 1 request, got %d", len(result))
	}

	want := mcreconcile.Request{
		Request: reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      "short-name",
				Namespace: "my-namespace",
			},
		},
		ClusterName: "/my/cluster",
	}

	for req := range result {
		if req != want {
			t.Errorf("got request %+v, want %+v", req, want)
		}
	}
}

func TestGetOwnerReconcileRequest_NoMatch(t *testing.T) {
	e := &enqueueRequestForOwner[client.Object]{
		groupKind: schema.GroupKind{Group: "dns.example.com", Kind: "DNSRecordSet"},
	}

	result := map[mcreconcile.Request]empty{}

	e.getOwnerReconcileRequest(&metav1.ObjectMeta{}, result)
	if len(result) != 0 {
		t.Errorf("expected 0 requests for empty object, got %d", len(result))
	}

	e.getOwnerReconcileRequest(&metav1.ObjectMeta{
		Annotations: ownerAnnotations("cluster-c1", "other.group", "OtherKind", "name", "ns"),
	}, result)
	if len(result) != 0 {
		t.Errorf("expected 0 requests for wrong group/kind, got %d", len(result))
	}
}
