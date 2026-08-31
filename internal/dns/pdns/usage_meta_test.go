// SPDX-License-Identifier: AGPL-3.0-only

package pdns

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/downstreamclient"
	"go.miloapis.com/dns-operator/internal/usage"
)

func TestEnsureZoneStampsUsageMetadata(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var putPath string
	var putBody metadataObject
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/servers/localhost/zones/example.com.", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "example.com."})
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/api/v1/servers/localhost/zones/example.com./metadata/DATUM-USAGE", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		mu.Lock()
		putPath = r.URL.Path
		if err := json.Unmarshal(body, &putBody); err != nil {
			mu.Unlock()
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "k")

	zone := dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "z1",
			Namespace: "ns-downstream",
			Annotations: map[string]string{
				downstreamclient.UpstreamOwnerClusterNameAnnotation: "cluster-_p-abc",
				downstreamclient.UpstreamOwnerGroupAnnotation:       usage.ResourceGroup,
				downstreamclient.UpstreamOwnerKindAnnotation:        usage.ResourceKind,
				downstreamclient.UpstreamOwnerNameAnnotation:        "z1",
				downstreamclient.UpstreamOwnerNamespaceAnnotation:   "project-ns",
				downstreamclient.UpstreamOwnerUIDAnnotation:         "uid-1",
			},
		},
		Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "example.com", DNSZoneClassName: "powerdns"},
	}
	require.NoError(t, c.EnsureZone(context.Background(), zone, dnsv1alpha1.DNSZoneClass{}))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "/api/v1/servers/localhost/zones/example.com./metadata/DATUM-USAGE", putPath)
	require.Len(t, putBody.Metadata, 1)
	id, ok := usage.UnmarshalIdentity("example.com.", putBody.Metadata[0])
	require.True(t, ok)
	assert.Equal(t, "p-abc", id.Project)
	assert.Equal(t, "z1", id.Name)
	assert.Equal(t, "project-ns", id.Namespace)
	assert.Equal(t, "uid-1", string(id.UID))
}

func TestListUsageIdentities(t *testing.T) {
	t.Parallel()

	payload, ok := usage.MarshalIdentity(usage.ZoneIdentity{
		Project: "p-abc", Name: "z1", Namespace: "ns", UID: "uid-1",
	})
	require.True(t, ok)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/servers/localhost/zones", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"name": "example.com."},
			{"name": "orphan.test."},
		})
	})
	mux.HandleFunc("/api/v1/servers/localhost/zones/example.com./metadata/DATUM-USAGE", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(metadataObject{Kind: usage.MetadataKind, Metadata: []string{payload}})
	})
	mux.HandleFunc("/api/v1/servers/localhost/zones/orphan.test./metadata/DATUM-USAGE", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "k")

	ids, err := c.ListUsageIdentities(context.Background())
	require.NoError(t, err)
	require.Len(t, ids, 1)
	assert.Equal(t, "p-abc", ids[0].Project)
	assert.Equal(t, "example.com.", ids[0].Domain)
}
