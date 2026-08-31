// SPDX-License-Identifier: AGPL-3.0-only

package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.miloapis.com/billing/emission"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/downstreamclient"
)

func TestCollectorObserveAndFlush(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, dnsv1alpha1.AddToScheme(scheme))
	zone := testZone("example.com", "p-abc", "z1", "project-ns", "uid-1")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone).Build()
	rec := &recordingRecorder{}

	c := &Collector{
		Client:   cl,
		Recorder: rec,
		Location: "us-east-1",
		Interval: time.Minute,
	}
	c.init()
	require.NoError(t, c.refreshIndex(context.Background()))

	resp := encodePBDNSMessage(pbTypeDNSResponse, "www.example.com.", 1, 0)
	query := encodePBDNSMessage(1, "www.example.com.", 1, 0)
	unhosted := encodePBDNSMessage(pbTypeDNSResponse, "other.org.", 1, 0)

	c.observe(resp)
	c.observe(resp)
	c.observe(query)    // questions are ignored
	c.observe(unhosted) // no matching zone

	c.flush(context.Background())
	events := rec.snapshot()
	require.Len(t, events, 1)
	assert.Equal(t, MeterZoneQueries, events[0].Meter)
	assert.Equal(t, int64(2), events[0].Quantity)
	assert.Equal(t, "p-abc", events[0].Project.Name)
	assert.Equal(t, "NOERROR", events[0].Dimensions[DimRcode])
	assert.Equal(t, "A", events[0].Dimensions[DimRecordType])
	assert.Equal(t, "us-east-1", events[0].Dimensions[DimLocation])
	require.NotNil(t, events[0].Resource)
	assert.Equal(t, ResourceGroup, events[0].Resource.Group)
	assert.Equal(t, ResourceKind, events[0].Resource.Kind)
	assert.Equal(t, "z1", events[0].Resource.Name)
	assert.Equal(t, "project-ns", events[0].Resource.Namespace)
	assert.Equal(t, "uid-1", string(events[0].Resource.UID))
}

func TestCollectorRefreshIndexFromStore(t *testing.T) {
	t.Parallel()
	rec := &recordingRecorder{}
	c := &Collector{
		Recorder: rec,
		Store: staticStore{ids: []ZoneIdentity{{
			Project:   "p-edge",
			Name:      "z-edge",
			Namespace: "ns",
			UID:       "uid-edge",
			Domain:    "edge.example.com",
		}}},
		Interval: time.Minute,
	}
	c.init()
	require.NoError(t, c.refreshIndex(context.Background()))

	c.observe(encodePBDNSMessage(pbTypeDNSResponse, "www.edge.example.com.", 1, 0))
	c.flush(context.Background())
	events := rec.snapshot()
	require.Len(t, events, 1)
	assert.Equal(t, "p-edge", events[0].Project.Name)
	assert.Equal(t, "z-edge", events[0].Resource.Name)
}

func TestCollectorRefreshIndexKubeWinsOverStore(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, dnsv1alpha1.AddToScheme(scheme))
	zone := testZone("example.com", "p-kube", "z-kube", "project-ns", "uid-kube")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone).Build()
	c := &Collector{
		Client: cl,
		Store: staticStore{ids: []ZoneIdentity{{
			Project:   "p-pdns",
			Name:      "z-pdns",
			Namespace: "ns",
			UID:       "uid-pdns",
			Domain:    "example.com",
		}}},
		Interval: time.Minute,
	}
	c.init()
	require.NoError(t, c.refreshIndex(context.Background()))
	id, ok := c.index.Lookup("www.example.com")
	require.True(t, ok)
	assert.Equal(t, "p-kube", id.Project)
	assert.Equal(t, "z-kube", id.Name)
}

type staticStore struct {
	ids []ZoneIdentity
	err error
}

func (s staticStore) ListUsageIdentities(context.Context) ([]ZoneIdentity, error) {
	return s.ids, s.err
}

func TestCollectorFlushRestoresOnError(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, dnsv1alpha1.AddToScheme(scheme))
	zone := testZone("example.com", "p-abc", "z1", "project-ns", "uid-1")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone).Build()
	rec := &recordingRecorder{err: errors.New("vector down")}

	c := &Collector{Client: cl, Recorder: rec, Interval: time.Minute}
	c.init()
	require.NoError(t, c.refreshIndex(context.Background()))
	c.observe(encodePBDNSMessage(pbTypeDNSResponse, "example.com.", 1, 0))
	c.flush(context.Background())
	assert.Empty(t, rec.snapshot())

	rec.err = nil
	c.flush(context.Background())
	events := rec.snapshot()
	require.Len(t, events, 1)
	assert.Equal(t, int64(1), events[0].Quantity)
}

func TestCollectorFlushDropsValidationError(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, dnsv1alpha1.AddToScheme(scheme))
	zone := testZone("example.com", "p-abc", "z1", "project-ns", "uid-1")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone).Build()
	rec := &recordingRecorder{err: &emission.ValidationError{Field: "Project.Name", Message: "must be a plain project name"}}

	c := &Collector{Client: cl, Recorder: rec, Interval: time.Minute}
	c.init()
	require.NoError(t, c.refreshIndex(context.Background()))
	c.observe(encodePBDNSMessage(pbTypeDNSResponse, "example.com.", 1, 0))
	c.flush(context.Background())
	assert.Empty(t, rec.snapshot())

	rec.err = nil
	c.flush(context.Background())
	assert.Empty(t, rec.snapshot())
}

func TestCollectorHandleConn(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, dnsv1alpha1.AddToScheme(scheme))
	zone := testZone("example.com", "p-abc", "z1", "project-ns", "uid-1")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone).Build()
	rec := &recordingRecorder{}

	c := &Collector{Client: cl, Recorder: rec, Interval: time.Minute}
	c.init()
	require.NoError(t, c.refreshIndex(context.Background()))

	server, client := net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})

	done := make(chan struct{})
	go func() {
		c.handleConn(context.Background(), server)
		close(done)
	}()

	payload := encodePBDNSMessage(pbTypeDNSResponse, "example.com.", 28, 3)
	require.NoError(t, writeLengthPrefixed(client, payload))
	require.NoError(t, client.Close())

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleConn did not return")
	}

	c.flush(context.Background())
	events := rec.snapshot()
	require.Len(t, events, 1)
	assert.Equal(t, "AAAA", events[0].Dimensions[DimRecordType])
	assert.Equal(t, "NXDOMAIN", events[0].Dimensions[DimRcode])
}

func TestCollectorSkipsZeroQuantity(t *testing.T) {
	t.Parallel()
	rec := &recordingRecorder{}
	c := &Collector{Recorder: rec, Interval: time.Minute}
	c.init()
	c.counters.counts[queryKey{domain: "example.com", rcode: "NOERROR", recordType: "A"}] = 0
	c.flush(context.Background())
	assert.Empty(t, rec.snapshot())
}

func TestReadLengthPrefixedEOF(t *testing.T) {
	t.Parallel()
	_, err := readLengthPrefixed(bytes.NewReader(nil))
	assert.ErrorIs(t, err, io.EOF)
}

func TestCollectorFlushHTTP(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, dnsv1alpha1.AddToScheme(scheme))
	zone := testZone("example.com", "p-abc", "z1", "project-ns", "uid-1")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone).Build()

	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	rec, err := emission.NewUsageRecorder(
		emission.WithEndpoint(srv.URL),
		emission.WithHTTPClient(srv.Client()),
		emission.WithRetryPolicy(emission.RetryPolicy{MaxAttempts: 1}),
	)
	require.NoError(t, err)

	c := &Collector{Client: cl, Recorder: rec, Location: "us-east-1", Interval: time.Minute}
	c.init()
	require.NoError(t, c.refreshIndex(context.Background()))
	c.observe(encodePBDNSMessage(pbTypeDNSResponse, "www.example.com.", 1, 0))
	c.flush(context.Background())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 1)

	var ce map[string]any
	require.NoError(t, json.Unmarshal(bodies[0], &ce))
	assert.Equal(t, MeterZoneQueries, ce["type"])
	assert.Equal(t, SourceURI, ce["source"])
	assert.Equal(t, "projects/p-abc", ce["subject"])

	data, ok := ce["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "1", data["value"])
	dims, ok := data["dimensions"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "NOERROR", dims[DimRcode])
	assert.Equal(t, "A", dims[DimRecordType])
	assert.Equal(t, "us-east-1", dims[DimLocation])
	res, ok := data["resource"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, ResourceGroup, res["group"])
	assert.Equal(t, ResourceKind, res["kind"])
	assert.Equal(t, "z1", res["name"])
	assert.Equal(t, "project-ns", res["namespace"])
	assert.Equal(t, "uid-1", res["uid"])
}

func TestCollectorFlushHTTPSlashEncodedProject(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, dnsv1alpha1.AddToScheme(scheme))
	zone := testZone("example.com", "x", "z1", "project-ns", "uid-1")
	zone.Annotations[downstreamclient.UpstreamOwnerClusterNameAnnotation] = "cluster-_p-abc"
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone).Build()

	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	rec, err := emission.NewUsageRecorder(
		emission.WithEndpoint(srv.URL),
		emission.WithHTTPClient(srv.Client()),
		emission.WithRetryPolicy(emission.RetryPolicy{MaxAttempts: 1}),
	)
	require.NoError(t, err)

	c := &Collector{Client: cl, Recorder: rec, Interval: time.Minute}
	c.init()
	require.NoError(t, c.refreshIndex(context.Background()))
	c.observe(encodePBDNSMessage(pbTypeDNSResponse, "www.example.com.", 1, 0))
	c.flush(context.Background())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 1)

	var ce map[string]any
	require.NoError(t, json.Unmarshal(bodies[0], &ce))
	assert.Equal(t, "projects/p-abc", ce["subject"])
}
