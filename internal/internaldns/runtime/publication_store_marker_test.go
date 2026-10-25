// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
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
	if err := claimPublicationStore(ctx, api, "platform", publicationStoreKubernetes); err != nil {
		t.Fatalf("first replica: %v", err)
	}
	if err := claimPublicationStore(ctx, api, "platform", publicationStoreKubernetes); err != nil {
		t.Fatalf("second replica on the same store: %v", err)
	}
	if err := claimPublicationStore(ctx, api, "platform", publicationStoreJetStream); err == nil {
		t.Fatal("a replica on another store started beside the active one")
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
