// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"testing"

	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
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
