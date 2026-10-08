// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"reflect"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

type isolationCluster struct {
	cluster.Cluster
	client client.Client
}

func (c *isolationCluster) GetClient() client.Client { return c.client }
func (c *isolationCluster) GetConfig() *rest.Config {
	return &rest.Config{Host: "http://127.0.0.1:1"}
}

type isolationManager struct {
	mcmanager.Manager
	upstream cluster.Cluster
}

func (m *isolationManager) GetCluster(context.Context, multicluster.ClusterName) (cluster.Cluster, error) {
	return m.upstream, nil
}

func TestPublicControllersSkipPrivateZones(t *testing.T) {
	zoneKey := client.ObjectKey{Namespace: "project-a", Name: "private-zone"}
	recordKey := client.ObjectKey{Namespace: zoneKey.Namespace, Name: "api"}
	discoveryKey := client.ObjectKey{Namespace: zoneKey.Namespace, Name: "discover"}
	tests := []struct {
		name      string
		reconcile func(context.Context, client.Client, mcmanager.Manager) (ctrl.Result, error)
	}{
		{"zone serving", func(ctx context.Context, cl client.Client, _ mcmanager.Manager) (ctrl.Result, error) {
			return (&DNSZoneReconciler{Client: cl}).Reconcile(ctx, ctrl.Request{NamespacedName: zoneKey})
		}},
		{"record serving", func(ctx context.Context, cl client.Client, _ mcmanager.Manager) (ctrl.Result, error) {
			return (&DNSRecordSetReconciler{Client: cl}).Reconcile(ctx, ctrl.Request{NamespacedName: recordKey})
		}},
		{"PowerDNS serving", func(ctx context.Context, cl client.Client, _ mcmanager.Manager) (ctrl.Result, error) {
			return (&DNSRecordSetPowerDNSReconciler{Client: cl}).Reconcile(ctx, PowerDNSRecordSetReconcileRequest{
				Request: ctrl.Request{NamespacedName: zoneKey}, RecordSetType: "AAAA", RecordSetName: "api",
			})
		}},
		{"zone replication", func(ctx context.Context, _ client.Client, mgr mcmanager.Manager) (ctrl.Result, error) {
			return (&DNSZoneReplicator{mgr: mgr}).Reconcile(ctx, mcreconcile.Request{
				ClusterName: "project-a", Request: ctrl.Request{NamespacedName: zoneKey},
			})
		}},
		{"record replication", func(ctx context.Context, _ client.Client, mgr mcmanager.Manager) (ctrl.Result, error) {
			return (&DNSRecordSetReplicator{mgr: mgr}).Reconcile(ctx, mcreconcile.Request{
				ClusterName: "project-a", Request: ctrl.Request{NamespacedName: recordKey},
			})
		}},
		{"public discovery", func(ctx context.Context, _ client.Client, mgr mcmanager.Manager) (ctrl.Result, error) {
			return (&DNSZoneDiscoveryReplicator{mgr: mgr}).Reconcile(ctx, mcreconcile.Request{
				ClusterName: "project-a", Request: ctrl.Request{NamespacedName: discoveryKey},
			})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			zone := &dnsv1alpha1.DNSZone{
				ObjectMeta: metav1.ObjectMeta{Name: zoneKey.Name, Namespace: zoneKey.Namespace, UID: "zone-uid"},
				Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "prod.example.com", DNSZoneClassName: "public-class",
					Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate},
			}
			record := &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{Name: recordKey.Name, Namespace: recordKey.Namespace},
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name}, RecordType: dnsv1alpha1.RRTypeAAAA,
				},
			}
			discovery := &dnsv1alpha1.DNSZoneDiscovery{
				ObjectMeta: metav1.ObjectMeta{Name: discoveryKey.Name, Namespace: discoveryKey.Namespace},
				Spec:       dnsv1alpha1.DNSZoneDiscoverySpec{DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name}},
			}
			objects := []client.Object{zone, record, discovery}
			cl := fake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(objects...).
				WithStatusSubresource(objects...).Build()
			mgr := &isolationManager{upstream: &isolationCluster{client: cl}}
			ctx := context.Background()
			before := make([]client.Object, len(objects))
			for i, obj := range objects {
				if err := cl.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
					t.Fatal(err)
				}
				before[i] = obj.DeepCopyObject().(client.Object)
			}
			// Omit public backends, downstream clients, and discovery configuration.
			// Private resources must be rejected before any of them are used.
			for attempt := 0; attempt < 2; attempt++ {
				result, err := test.reconcile(ctx, cl, mgr)
				if err != nil || result != (ctrl.Result{}) {
					t.Fatalf("private resource was not ignored: result=%+v err=%v", result, err)
				}
			}
			for i, obj := range objects {
				if err := cl.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(obj, before[i]) {
					t.Fatalf("public controller modified %T: before=%+v after=%+v", obj, before[i], obj)
				}
			}
		})
	}
}

func TestPublicRecordReplicationRetainsFinalizer(t *testing.T) {
	for _, visibility := range []dnsv1alpha1.DNSZoneVisibility{"", dnsv1alpha1.DNSZoneVisibilityPublic} {
		t.Run("visibility="+string(visibility), func(t *testing.T) {
			t.Parallel()
			zone := &dnsv1alpha1.DNSZone{
				ObjectMeta: metav1.ObjectMeta{Name: "public-zone", Namespace: "project-a"},
				Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "example.com", DNSZoneClassName: "public-class",
					Visibility: visibility},
			}
			record := &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: zone.Namespace},
				Spec:       dnsv1alpha1.DNSRecordSetSpec{DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name}},
			}
			cl := fake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(zone, record).Build()
			mgr := &isolationManager{upstream: &isolationCluster{client: cl}}
			req := mcreconcile.Request{ClusterName: "project-a",
				Request: ctrl.Request{NamespacedName: client.ObjectKeyFromObject(record)}}
			if _, err := (&DNSRecordSetReplicator{mgr: mgr}).Reconcile(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(context.Background(), client.ObjectKeyFromObject(record), record); err != nil {
				t.Fatal(err)
			}
			if len(record.Finalizers) != 1 || record.Finalizers[0] != rsFinalizer {
				t.Fatalf("public record lost its deletion finalizer: %v", record.Finalizers)
			}
		})
	}
}

func TestRecordCreatedBeforePrivateZoneAvoidsPublicFinalizer(t *testing.T) {
	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "private-zone", Namespace: "project-a"},
		Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "prod.example.com", DNSZoneClassName: "public-class",
			Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate},
	}
	record := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: zone.Namespace},
		Spec:       dnsv1alpha1.DNSRecordSetSpec{DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name}},
	}
	cl := fake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(record).
		WithStatusSubresource(record).Build()
	mgr := &isolationManager{upstream: &isolationCluster{client: cl}}
	reconciler := &DNSRecordSetReplicator{mgr: mgr}
	req := mcreconcile.Request{ClusterName: "project-a",
		Request: ctrl.Request{NamespacedName: client.ObjectKeyFromObject(record)}}
	ctx := context.Background()
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(record), record); err != nil {
		t.Fatal(err)
	}
	if len(record.Finalizers) != 0 || !apimeta.IsStatusConditionFalse(record.Status.Conditions, CondAccepted) {
		t.Fatalf("unresolved zone must wait without a public finalizer: %+v", record)
	}
	before := record.DeepCopy()
	if err := cl.Create(ctx, zone); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(record), record); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(record, before) {
		t.Fatalf("private zone reconciliation modified a waiting record: %+v", record)
	}
}

func TestPublicRecordDeletionCompletesAfterZoneDisappears(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "project-a", UID: "project-uid"}}
	record := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: namespace.Name, UID: "record-uid",
			Finalizers: []string{rsFinalizer}, DeletionTimestamp: &metav1.Time{Time: time.Now()}},
		Spec: dnsv1alpha1.DNSRecordSetSpec{DNSZoneRef: corev1.LocalObjectReference{Name: "deleted-zone"}},
	}
	scheme := newTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, record).Build()
	downstream := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &isolationManager{upstream: &isolationCluster{client: cl}}
	req := mcreconcile.Request{ClusterName: "project-a",
		Request: ctrl.Request{NamespacedName: client.ObjectKeyFromObject(record)}}
	ctx := context.Background()
	if _, err := (&DNSRecordSetReplicator{mgr: mgr, DownstreamClient: downstream}).Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(record), record); !apierrors.IsNotFound(err) {
		t.Fatalf("public record deletion did not finish after its zone disappeared: %v", err)
	}
}
