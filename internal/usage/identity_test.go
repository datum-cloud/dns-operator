// SPDX-License-Identifier: AGPL-3.0-only

package usage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/downstreamclient"
)

func testZone(domain, project, name, namespace, uid string) *dnsv1alpha1.DNSZone {
	return &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns-downstream",
			Annotations: map[string]string{
				downstreamclient.UpstreamOwnerClusterNameAnnotation: "cluster-" + project,
				downstreamclient.UpstreamOwnerGroupAnnotation:       ResourceGroup,
				downstreamclient.UpstreamOwnerKindAnnotation:        ResourceKind,
				downstreamclient.UpstreamOwnerNameAnnotation:        name,
				downstreamclient.UpstreamOwnerNamespaceAnnotation:   namespace,
				downstreamclient.UpstreamOwnerUIDAnnotation:         uid,
			},
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       domain,
			DNSZoneClassName: "powerdns",
		},
	}
}

func TestIdentityFromZone(t *testing.T) {
	t.Parallel()

	t.Run("attributed zone", func(t *testing.T) {
		zone := testZone("Example.COM.", "p-abc", "example-zone", "project-ns", "uid-1")
		id, ok := IdentityFromZone(zone)
		require.True(t, ok)
		assert.Equal(t, "p-abc", id.Project)
		assert.Equal(t, "example-zone", id.Name)
		assert.Equal(t, "project-ns", id.Namespace)
		assert.Equal(t, types.UID("uid-1"), id.UID)
		assert.Equal(t, "Example.COM.", id.Domain)
	})

	t.Run("slash encoded milo cluster name", func(t *testing.T) {
		zone := testZone("example.com", "x", "z1", "ns", "uid")
		zone.Annotations[downstreamclient.UpstreamOwnerClusterNameAnnotation] = "cluster-_p-abc"
		id, ok := IdentityFromZone(zone)
		require.True(t, ok)
		assert.Equal(t, "p-abc", id.Project)
	})

	t.Run("nested slash is not a billing project", func(t *testing.T) {
		zone := testZone("example.com", "x", "z1", "ns", "uid")
		zone.Annotations[downstreamclient.UpstreamOwnerClusterNameAnnotation] = "cluster-_my_cluster"
		_, ok := IdentityFromZone(zone)
		assert.False(t, ok)
	})

	t.Run("missing project", func(t *testing.T) {
		zone := testZone("example.com", "p-abc", "z1", "ns", "uid")
		delete(zone.Annotations, downstreamclient.UpstreamOwnerClusterNameAnnotation)
		_, ok := IdentityFromZone(zone)
		assert.False(t, ok)
	})

	t.Run("missing domain", func(t *testing.T) {
		zone := testZone("", "p-abc", "z1", "ns", "uid")
		_, ok := IdentityFromZone(zone)
		assert.False(t, ok)
	})

	t.Run("missing uid", func(t *testing.T) {
		zone := testZone("example.com", "p-abc", "z1", "ns", "uid")
		delete(zone.Annotations, downstreamclient.UpstreamOwnerUIDAnnotation)
		_, ok := IdentityFromZone(zone)
		assert.False(t, ok)
	})
}

func TestMarshalUnmarshalIdentity(t *testing.T) {
	t.Parallel()
	id := ZoneIdentity{Project: "p-abc", Name: "z1", Namespace: "ns", UID: "uid-1", Domain: "example.com"}
	raw, ok := MarshalIdentity(id)
	require.True(t, ok)
	assert.NotContains(t, raw, "example.com", "domain is the PDNS zone key, not a JSON field")

	got, ok := UnmarshalIdentity("Example.COM.", raw)
	require.True(t, ok)
	assert.Equal(t, "p-abc", got.Project)
	assert.Equal(t, "z1", got.Name)
	assert.Equal(t, "ns", got.Namespace)
	assert.Equal(t, types.UID("uid-1"), got.UID)
	assert.Equal(t, "Example.COM.", got.Domain)

	_, ok = MarshalIdentity(ZoneIdentity{Project: "p-abc", Name: "z1", Namespace: "ns"})
	assert.False(t, ok)
	_, ok = UnmarshalIdentity("example.com", `{"project":"p/abc","name":"z1","namespace":"ns","uid":"u"}`)
	assert.False(t, ok)
	_, ok = UnmarshalIdentity("", raw)
	assert.False(t, ok)
}

func TestNormalizeDomain(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "example.com", NormalizeDomain("Example.COM."))
	assert.Equal(t, "example.com", NormalizeDomain(" example.com "))
}

func TestZoneIndexLookup(t *testing.T) {
	t.Parallel()
	idx := &ZoneIndex{}
	idx.Replace([]ZoneIdentity{
		{Domain: "example.com", Project: "p1", Name: "apex"},
		{Domain: "www.example.com", Project: "p1", Name: "www"},
	})

	tests := []struct {
		qname string
		want  string
		ok    bool
	}{
		{qname: "www.example.com.", want: "www", ok: true},
		{qname: "WWW.EXAMPLE.COM", want: "www", ok: true},
		{qname: "api.example.com", want: "apex", ok: true},
		{qname: "example.com", want: "apex", ok: true},
		{qname: "example.org", want: "", ok: false},
		{qname: "notexample.com", want: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.qname, func(t *testing.T) {
			id, ok := idx.Lookup(tt.qname)
			assert.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.Equal(t, tt.want, id.Name)
			}
		})
	}
}

func TestRecordTypeAndRcodeNames(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "A", recordTypeName(1))
	assert.Equal(t, "AAAA", recordTypeName(28))
	assert.Equal(t, "NOERROR", rcodeName(0))
	assert.Equal(t, "NXDOMAIN", rcodeName(3))
	assert.Equal(t, "SERVFAIL", rcodeName(2))
	assert.Equal(t, "SERVFAIL", rcodeName(65536))
	assert.Equal(t, "RCODE99", rcodeName(99))
}

func TestDimensionsOmitsEmptyLocation(t *testing.T) {
	t.Parallel()
	assert.Nil(t, dimensions("", nil))
	assert.Equal(t, map[string]string{DimRcode: "NOERROR"}, dimensions("", map[string]string{DimRcode: "NOERROR"}))
	assert.Equal(t, map[string]string{DimLocation: "us-east-1"}, dimensions("us-east-1", nil))
}

func TestEventForZone(t *testing.T) {
	t.Parallel()
	zone := ZoneIdentity{
		Project:   "p-abc",
		Name:      "z1",
		Namespace: "ns",
		UID:       "uid-1",
		Domain:    "example.com",
	}
	ev := eventForZone(MeterZoneQueries, zone, 7, "us-east-1", map[string]string{
		DimRcode:      "NOERROR",
		DimRecordType: "A",
	}, metav1.Now().Time)

	assert.Equal(t, MeterZoneQueries, ev.Meter)
	assert.Equal(t, "p-abc", ev.Project.Name)
	assert.Equal(t, SourceURI, ev.Source)
	assert.Equal(t, int64(7), ev.Quantity)
	assert.Equal(t, "NOERROR", ev.Dimensions[DimRcode])
	assert.Equal(t, "A", ev.Dimensions[DimRecordType])
	assert.Equal(t, "us-east-1", ev.Dimensions[DimLocation])
	require.NotNil(t, ev.Resource)
	assert.Equal(t, ResourceGroup, ev.Resource.Group)
	assert.Equal(t, ResourceKind, ev.Resource.Kind)
	assert.Equal(t, "z1", ev.Resource.Name)
	assert.Equal(t, "ns", ev.Resource.Namespace)
	assert.Equal(t, types.UID("uid-1"), ev.Resource.UID)
}

func TestProjectNameFromOwnerMeta(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "p-abc", downstreamclient.ProjectNameFromOwnerMeta(map[string]string{
		downstreamclient.UpstreamOwnerClusterNameAnnotation: "cluster-p-abc",
	}))
	assert.Equal(t, "/my/cluster", downstreamclient.ProjectNameFromOwnerMeta(map[string]string{
		downstreamclient.UpstreamOwnerClusterNameAnnotation: "cluster-_my_cluster",
	}))
	assert.Equal(t, "", downstreamclient.ProjectNameFromOwnerMeta(nil))
}
