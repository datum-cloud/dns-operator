// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"testing"

	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestControlPlaneRefusesAStoreOtherThanTheActiveOne(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	api := fake.NewClientBuilder().WithScheme(s).Build()
	ctx := t.Context()
	if err := claimPublicationStore(ctx, api, "platform", publicationStoreKubernetes, false); err != nil {
		t.Fatalf("first replica: %v", err)
	}
	if err := claimPublicationStore(ctx, api, "platform", publicationStoreKubernetes, false); err != nil {
		t.Fatalf("second replica on the same store: %v", err)
	}
	if err := claimPublicationStore(ctx, api, "platform", publicationStoreJetStream, false); err == nil {
		t.Fatal("a replica on another store started beside the active one")
	}
}

func TestFirstJetStreamStartRefusesExistingAPIRecords(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := dnsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	lease := &dnsv1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-a", Namespace: "platform"}}
	api := fake.NewClientBuilder().WithScheme(s).WithObjects(lease).Build()
	if err := claimPublicationStore(t.Context(), api, "platform", publicationStoreJetStream, false); err == nil {
		t.Fatal("switched to JetStream without the active-store marker while API leases exist")
	}
	if err := claimPublicationStore(t.Context(), api, "platform", publicationStoreKubernetes, false); err != nil {
		t.Fatalf("API store on an upgraded cluster: %v", err)
	}
}

func TestPublicationStoreConfigNeedsARegionalTransport(t *testing.T) {
	for name, store := range map[string]PublicationStoreConfig{
		"no region":    {Bucket: "DNS_PRIVATE_PUBLICATIONS"},
		"bad bucket":   {Region: "east", Bucket: "dns.publications"},
		"no transport": {Region: "west", Bucket: "DNS_PRIVATE_PUBLICATIONS"},
		"bad replicas": {Region: "east", Bucket: "DNS_PRIVATE_PUBLICATIONS", Replicas: -1},
	} {
		c := Config{RegionalTransports: []RegionalTransportConfig{{Region: "east"}}, PublicationStore: &store}
		if err := c.validatePublicationStore(); err == nil {
			t.Errorf("%s: accepted %#v", name, store)
		}
	}
	c := Config{RegionalTransports: []RegionalTransportConfig{{Region: "east"}}, PublicationStore: &PublicationStoreConfig{Region: "east", Bucket: "DNS_PRIVATE_PUBLICATIONS", Replicas: 3}}
	if err := c.validatePublicationStore(); err != nil {
		t.Errorf("valid store rejected: %v", err)
	}
}

func TestAStopThenStartRolloutSwitchesTheActiveStore(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := dnsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	lease := &dnsv1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-a", Namespace: "platform"}}
	api := fake.NewClientBuilder().WithScheme(s).WithObjects(lease).Build()
	ctx := t.Context()
	if err := claimPublicationStore(ctx, api, "platform", publicationStoreKubernetes, false); err != nil {
		t.Fatal(err)
	}
	if err := claimPublicationStore(ctx, api, "platform", publicationStoreJetStream, true); err != nil {
		t.Fatalf("switching rollout refused: %v", err)
	}
	if err := claimPublicationStore(ctx, api, "platform", publicationStoreKubernetes, false); err == nil {
		t.Fatal("an old replica started after the switch")
	}
	if err := claimPublicationStore(ctx, api, "platform", publicationStoreJetStream, false); err != nil {
		t.Fatalf("replica on the switched store: %v", err)
	}
}

func TestRegionalStoresCannotSwitchBack(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := dnsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	api := fake.NewClientBuilder().WithScheme(s).Build()
	ctx := t.Context()
	if err := claimPublicationStore(ctx, api, "platform", publicationStoreJetStream, false); err != nil {
		t.Fatal(err)
	}
	if err := claimPublicationStore(ctx, api, "platform", publicationStoreRegional, true); err != nil {
		t.Fatalf("a stopped rollout must switch to regional stores: %v", err)
	}
	if err := claimPublicationStore(ctx, api, "platform", publicationStoreRegional, true); err != nil {
		t.Fatalf("a second region's control plane on regional stores: %v", err)
	}
	if err := claimPublicationStore(ctx, api, "platform", publicationStoreJetStream, true); err == nil {
		t.Fatal("switched back from regional stores to the shared writer")
	}
}
