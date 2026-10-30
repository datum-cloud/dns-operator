// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func addReferenceStateToScheme(scheme *runtime.Scheme) error {
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	return dnsv1alpha1.AddToScheme(scheme)
}

func seedReferenceState(t *testing.T, r *Reconciler, source client.Object, selected map[string]dnsv1alpha1.DNSObjectReference) {
	t.Helper()
	source = source.DeepCopyObject().(client.Object)
	switch object := source.(type) {
	case *dnsv1alpha1.DNSZoneAssociation:
		object.Status = dnsv1alpha1.DNSZoneAssociationStatus{}
	case *dnsv1alpha1.DNSNamingPolicy:
		object.Status = dnsv1alpha1.DNSNamingPolicyStatus{}
	case *dnsv1alpha1.DNSRegistration:
		object.Status = dnsv1alpha1.DNSRegistrationStatus{}
	}
	if _, valid, err := r.pinReferences(context.Background(), source, selected); err != nil || !valid {
		t.Fatalf("reference state: valid=%t err=%v", valid, err)
	}
}

func TestReferenceStateIsPrivateScopedAndCollected(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := addReferenceStateToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &Reconciler{Client: cl, Options: Options{ProjectUID: "project-a", PlatformNamespace: "platform"}}
	other := &Reconciler{Client: cl, Options: Options{ProjectUID: "project-b", PlatformNamespace: "platform"}}
	source := &dnsv1alpha1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "project", UID: "registration-original"}}
	selected := map[string]dnsv1alpha1.DNSObjectReference{"zone": {Name: "private", UID: "zone-original", Generation: 1}}
	if _, valid, err := r.pinReferences(ctx, source, selected); err != nil || !valid {
		t.Fatalf("initial reference: valid=%t err=%v", valid, err)
	}
	if _, valid, err := other.pinReferences(ctx, source, selected); err != nil || !valid {
		t.Fatalf("other project reference: valid=%t err=%v", valid, err)
	}
	selected["zone"] = dnsv1alpha1.DNSObjectReference{Name: "private", UID: "zone-replacement", Generation: 1}
	pins, valid, err := r.pinReferences(ctx, source, selected)
	if err != nil || valid || pins["zone"].UID != "zone-original" {
		t.Fatalf("replacement transferred a resolved lifetime: pins=%v valid=%t err=%v", pins, valid, err)
	}
	var public corev1.ConfigMapList
	if err := cl.List(ctx, &public, client.InNamespace(source.Namespace)); err != nil || len(public.Items) != 0 {
		t.Fatalf("reference state appeared in the project: count=%d err=%v", len(public.Items), err)
	}
	if err := r.collectReferenceStates(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var states corev1.ConfigMapList
	if err := cl.List(ctx, &states, client.InNamespace("platform")); err != nil || len(states.Items) != 1 || states.Items[0].Labels[referenceProjectLabel] != "project-b" {
		t.Fatalf("cleanup removed another project's state: states=%v err=%v", states.Items, err)
	}
	if err := cl.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	if err := other.collectReferenceStates(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := cl.List(ctx, &states, client.InNamespace("platform")); err != nil || len(states.Items) != 1 {
		t.Fatalf("collector removed a live source absent from the list snapshot: count=%d err=%v", len(states.Items), err)
	}
	if err := cl.Delete(ctx, source); err != nil {
		t.Fatal(err)
	}
	if err := other.collectReferenceStates(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := cl.List(ctx, &states, client.InNamespace("platform")); err != nil || len(states.Items) != 0 {
		t.Fatalf("deleted source retained private state: count=%d err=%v", len(states.Items), err)
	}
}

func TestMissingReferenceStateDoesNotReattachObservedResource(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := addReferenceStateToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &Reconciler{Client: cl, Options: Options{ProjectUID: "project", PlatformNamespace: "platform"}}
	source := &dnsv1alpha1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "project", UID: "registration"}, Status: dnsv1alpha1.DNSRegistrationStatus{CanonicalFQDN: "web.datum.internal."}}
	selected := map[string]dnsv1alpha1.DNSObjectReference{"zone": {Name: "private", UID: "replacement-zone"}}
	for range 2 {
		pins, valid, err := r.pinReferences(ctx, source, selected)
		if err != nil || valid || pins["zone"].UID != "" {
			t.Fatalf("unverified reference reattached: pins=%v valid=%t err=%v", pins, valid, err)
		}
		source.Status = dnsv1alpha1.DNSRegistrationStatus{}
	}
	source.UID = "recreated-registration"
	if _, valid, err := r.pinReferences(ctx, source, selected); err != nil || !valid {
		t.Fatalf("new registration could not establish references: valid=%t err=%v", valid, err)
	}
}

func TestReferenceCollectionSkipsMalformedOrForeignObjects(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := addReferenceStateToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &Reconciler{Client: cl, Options: Options{ProjectUID: "project", PlatformNamespace: "platform"}}
	source := &dnsv1alpha1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "project", UID: "registration"}}
	seedReferenceState(t, r, source, map[string]dnsv1alpha1.DNSObjectReference{"zone": {Name: "private", UID: "zone"}})
	for name, data := range map[string]string{"broken": "not-json", "foreign": `{"sourceKind":"DNSRegistration","sourceNamespace":"project","source":{"name":"web","uid":"foreign"}}`, "unknown": `{"sourceKind":"Other"}`} {
		object := r.referenceStateObject(source)
		object.Name = name
		object.Data = map[string]string{referenceStateDataKey: data}
		if err := cl.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.collectReferenceStates(ctx, nil); err != nil {
		t.Fatalf("unrelated reference state blocked collection: %v", err)
	}
	var states corev1.ConfigMapList
	if err := cl.List(ctx, &states, client.InNamespace("platform")); err != nil || len(states.Items) != 3 {
		t.Fatalf("valid retired state not collected, or foreign state deleted: count=%d err=%v", len(states.Items), err)
	}
}

func TestConcurrentReferenceCreationPreservesWinningIdentity(t *testing.T) {
	for _, different := range []bool{false, true} {
		t.Run(fmt.Sprintf("different_identity=%t", different), func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := addReferenceStateToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			arrived := make(chan struct{}, 2)
			release := make(chan struct{})
			var reads atomic.Int32
			cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.ConfigMap); ok && reads.Add(1) <= 2 {
						arrived <- struct{}{}
						<-release
						return apierrors.NewNotFound(corev1.Resource("configmaps"), key.Name)
					}
					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()
			r := &Reconciler{Client: cl, Options: Options{ProjectUID: "project", PlatformNamespace: "platform"}}
			source := &dnsv1alpha1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "project", UID: "registration"}}
			type result struct {
				pins  map[string]dnsv1alpha1.DNSObjectReference
				valid bool
				err   error
			}
			results := make(chan result, 2)
			for i := range 2 {
				go func() {
					uid := types.UID("zone-original")
					if different && i == 1 {
						uid = "zone-replacement"
					}
					pins, valid, err := r.pinReferences(ctx, source, map[string]dnsv1alpha1.DNSObjectReference{"zone": {Name: "private", UID: uid}})
					results <- result{pins, valid, err}
				}()
			}
			<-arrived
			<-arrived
			close(release)
			first, second := <-results, <-results
			if first.err != nil || second.err != nil {
				t.Fatalf("concurrent creation failed: %v, %v", first.err, second.err)
			}
			if first.pins["zone"].UID != second.pins["zone"].UID {
				t.Fatalf("replicas accepted different identities: %v, %v", first.pins, second.pins)
			}
			if different {
				if first.valid == second.valid {
					t.Fatalf("exactly one competing identity must be accepted: %t, %t", first.valid, second.valid)
				}
			} else if !first.valid || !second.valid {
				t.Fatal("equivalent concurrent reference was rejected")
			}
			stored, err := r.readReferences(ctx, source)
			if err != nil || stored["zone"].UID != first.pins["zone"].UID {
				t.Fatalf("winning identity was overwritten: %v, %v", stored, err)
			}
		})
	}
}

func TestConcurrentReferenceCreationRejectsForeignSource(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := addReferenceStateToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			foreign := obj.DeepCopyObject().(*corev1.ConfigMap)
			var state referenceState
			if err := json.Unmarshal([]byte(foreign.Data[referenceStateDataKey]), &state); err != nil {
				return err
			}
			state.Source.UID = "foreign-registration"
			payload, err := json.Marshal(state)
			if err != nil {
				return err
			}
			foreign.Data[referenceStateDataKey] = string(payload)
			if err := c.Create(ctx, foreign, opts...); err != nil {
				return err
			}
			return apierrors.NewAlreadyExists(corev1.Resource("configmaps"), obj.GetName())
		},
	}).Build()
	r := &Reconciler{Client: cl, Options: Options{ProjectUID: "project", PlatformNamespace: "platform"}}
	source := &dnsv1alpha1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "project", UID: "registration"}}
	_, valid, err := r.pinReferences(context.Background(), source, map[string]dnsv1alpha1.DNSObjectReference{"zone": {Name: "private", UID: "zone"}})
	if err == nil || valid || !strings.Contains(err.Error(), "source does not match") {
		t.Fatalf("foreign reference state accepted: valid=%t err=%v", valid, err)
	}
}
