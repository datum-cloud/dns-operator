// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"slices"
	"testing"

	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCopiesReadWithTheirOriginUID(t *testing.T) {
	s := k8sruntime.NewScheme()
	if err := dnsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	copied := &dnsv1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns-a", UID: "local-uid", Generation: 1, Annotations: map[string]string{originUIDAnnotation: "project-uid", originGenerationAnnotation: "9"}}}
	own := &dnsv1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "local", Namespace: "ns-a", UID: "own-uid"}}
	c := originUIDClient{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(copied, own).Build()}
	var got dnsv1.DNSRegistration
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(copied), &got); err != nil {
		t.Fatal(err)
	}
	if got.UID != "project-uid" || got.Generation != 9 {
		t.Fatalf("Get UID, generation = %s, %d, want the origin's", got.UID, got.Generation)
	}
	var list dnsv1.DNSRegistrationList
	if err := c.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	for _, item := range list.Items {
		if want := map[string]string{"web": "project-uid", "local": "own-uid"}[item.Name]; string(item.UID) != want {
			t.Fatalf("List UID for %s = %s, want %s", item.Name, item.UID, want)
		}
	}
}

func TestListDropsPeerCopiesThatShareAUID(t *testing.T) {
	s := k8sruntime.NewScheme()
	if err := dnsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	copyOf := func(name, origin, uid string) *dnsv1.DNSRegistration {
		return &dnsv1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns-a", UID: types.UID(name + "-local"), Labels: map[string]string{originLabel: origin}, Annotations: map[string]string{originUIDAnnotation: uid}}}
	}
	objs := []client.Object{
		copyOf("victim", originProject, "uid-a"),
		copyOf("alias", "us-east-1", "uid-a"),
		copyOf("peer-one", "us-east-1", "uid-b"),
		copyOf("peer-two", "us-west-1", "uid-b"),
		copyOf("peer", "us-east-1", "uid-c"),
	}
	c := originUIDClient{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()}
	var list dnsv1.DNSRegistrationList
	if err := c.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.Name)
	}
	slices.Sort(names)
	if want := []string{"peer", "victim"}; !slices.Equal(names, want) {
		t.Fatalf("List kept %v, want %v", names, want)
	}
}
