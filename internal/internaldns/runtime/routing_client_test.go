// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRoutingClientKeepsSourceAndPlatformObjectsSeparate(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	source := fake.NewClientBuilder().WithScheme(scheme).Build()
	platform := fake.NewClientBuilder().WithScheme(scheme).Build()
	routed := &RoutingClient{Client: platform, Source: source, PlatformNamespace: "internal-dns-system"}
	ctx := context.Background()
	projectObject := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "input", Namespace: "project-a"}}
	platformObject := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "manifest", Namespace: "internal-dns-system"}}
	if err := routed.Create(ctx, projectObject); err != nil {
		t.Fatal(err)
	}
	if err := routed.Create(ctx, platformObject); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		cl    client.Client
		key   client.ObjectKey
		found bool
	}{
		{"project in source", source, client.ObjectKeyFromObject(projectObject), true},
		{"project absent from platform", platform, client.ObjectKeyFromObject(projectObject), false},
		{"artifact in platform", platform, client.ObjectKeyFromObject(platformObject), true},
		{"artifact absent from source", source, client.ObjectKeyFromObject(platformObject), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cl.Get(ctx, tc.key, &corev1.ConfigMap{})
			if (err == nil) != tc.found {
				t.Fatalf("found=%v want=%v err=%v", err == nil, tc.found, err)
			}
		})
	}
}
