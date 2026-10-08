// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestDNSZoneVisibilityAdmission(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	cl, err := client.New(cfg, client.Options{Scheme: newTestScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	newZone := func(name string, visibility dnsv1alpha1.DNSZoneVisibility) *dnsv1alpha1.DNSZone {
		return &dnsv1alpha1.DNSZone{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "prod.example.com", DNSZoneClassName: "powerdns",
				Visibility: visibility},
		}
	}

	t.Run("omitted visibility defaults to public", func(t *testing.T) {
		zone := newZone("legacy", "")
		if err := cl.Create(ctx, zone); err != nil {
			t.Fatal(err)
		}
		if zone.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPublic {
			t.Fatalf("default visibility: %q", zone.Spec.Visibility)
		}
		zone.Spec.Visibility = ""
		if err := cl.Update(ctx, zone); err != nil {
			t.Fatalf("omitted public visibility must remain compatible: %v", err)
		}
	})
	t.Run("invalid visibility is rejected", func(t *testing.T) {
		if err := cl.Create(ctx, newZone("invalid", "Internal")); !apierrors.IsInvalid(err) {
			t.Fatalf("expected validation error, got %v", err)
		}
	})
	for _, visibility := range []dnsv1alpha1.DNSZoneVisibility{
		dnsv1alpha1.DNSZoneVisibilityPublic, dnsv1alpha1.DNSZoneVisibilityPrivate,
	} {
		t.Run("immutable "+string(visibility), func(t *testing.T) {
			zone := newZone("immutable-"+strings.ToLower(string(visibility)), visibility)
			if err := cl.Create(ctx, zone); err != nil {
				t.Fatal(err)
			}
			zone.Spec.DNSZoneClassName = "another-class"
			if err := cl.Update(ctx, zone); err != nil {
				t.Fatalf("unchanged visibility must allow other updates: %v", err)
			}
			if visibility == dnsv1alpha1.DNSZoneVisibilityPublic {
				zone.Spec.Visibility = dnsv1alpha1.DNSZoneVisibilityPrivate
			} else {
				zone.Spec.Visibility = dnsv1alpha1.DNSZoneVisibilityPublic
			}
			if err := cl.Update(ctx, zone); !apierrors.IsInvalid(err) {
				t.Fatalf("expected immutable visibility error, got %v", err)
			}
			if visibility == dnsv1alpha1.DNSZoneVisibilityPrivate {
				zone.Spec.Visibility = ""
				if err := cl.Update(ctx, zone); !apierrors.IsInvalid(err) {
					t.Fatalf("removing private visibility must not publish the zone: %v", err)
				}
			}
		})
	}
}
