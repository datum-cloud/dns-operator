// SPDX-License-Identifier: AGPL-3.0-only

package usage

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"go.miloapis.com/billing/emission"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/downstreamclient"
)

// ZoneIdentity is the billing identity of an upstream DNSZone, recovered
// from downstream shadow annotations.
type ZoneIdentity struct {
	Project   string
	Name      string
	Namespace string
	UID       types.UID
	Domain    string
}

// IdentityFromZone extracts billing identity from a (typically downstream)
// DNSZone. Returns false when the object cannot be attributed to a project
// or is missing a stable upstream UID.
func IdentityFromZone(zone *dnsv1alpha1.DNSZone) (ZoneIdentity, bool) {
	if zone == nil {
		return ZoneIdentity{}, false
	}
	meta := downstreamclient.OwnerMeta(zone)
	project := billingProjectName(meta)
	if project == "" || zone.Spec.DomainName == "" {
		return ZoneIdentity{}, false
	}

	id := ZoneIdentity{
		Project:   project,
		Name:      zone.Name,
		Namespace: zone.Namespace,
		Domain:    zone.Spec.DomainName,
	}
	if meta != nil {
		if v := meta[downstreamclient.UpstreamOwnerNameAnnotation]; v != "" {
			id.Name = v
		}
		if v := meta[downstreamclient.UpstreamOwnerNamespaceAnnotation]; v != "" {
			id.Namespace = v
		}
		id.UID = types.UID(meta[downstreamclient.UpstreamOwnerUIDAnnotation])
	}
	if id.UID == "" {
		return ZoneIdentity{}, false
	}
	return id, true
}

// billingProjectName is the Milo project the emission SDK accepts: a plain
// name with no slash. Cluster keys are stored as cluster-{name} with '/'
// encoded as '_', so "/p-abc" becomes cluster-_p-abc. The enqueue decoder
// restores the leading slash for ClusterName; billing must strip it.
func billingProjectName(ownerMeta map[string]string) string {
	project := strings.TrimPrefix(downstreamclient.ProjectNameFromOwnerMeta(ownerMeta), "/")
	if project == "" || strings.ContainsRune(project, '/') {
		return ""
	}
	return project
}

func dimensions(location string, extra map[string]string) map[string]string {
	n := len(extra)
	if location != "" {
		n++
	}
	if n == 0 {
		return nil
	}
	out := make(map[string]string, n)
	for k, v := range extra {
		if v != "" {
			out[k] = v
		}
	}
	if location != "" {
		out[DimLocation] = location
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// IdentityStore is the compact domain → billing map that travels with
// PowerDNS LMDB (LightningStream). Edge pods have no DNSZone CRs; they
// attribute queries from this store instead.
type IdentityStore interface {
	ListUsageIdentities(context.Context) ([]ZoneIdentity, error)
}

type identityPayload struct {
	Project   string `json:"project"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	UID       string `json:"uid"`
}

// MarshalIdentity encodes the compact identity stamped as PowerDNS
// domain metadata (kind DATUM-USAGE). Domain is the metadata's zone
// name, not a JSON field, so the edge lookup key always matches PDNS.
func MarshalIdentity(id ZoneIdentity) (string, bool) {
	if id.Project == "" || id.Name == "" || id.Namespace == "" || id.UID == "" {
		return "", false
	}
	if strings.ContainsRune(id.Project, '/') {
		return "", false
	}
	b, err := json.Marshal(identityPayload{
		Project:   id.Project,
		Name:      id.Name,
		Namespace: id.Namespace,
		UID:       string(id.UID),
	})
	if err != nil {
		return "", false
	}
	return string(b), true
}

// UnmarshalIdentity recovers billing identity from DATUM-USAGE metadata.
func UnmarshalIdentity(domain, raw string) (ZoneIdentity, bool) {
	var p identityPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return ZoneIdentity{}, false
	}
	if p.Project == "" || p.Name == "" || p.Namespace == "" || p.UID == "" {
		return ZoneIdentity{}, false
	}
	if strings.ContainsRune(p.Project, '/') {
		return ZoneIdentity{}, false
	}
	if NormalizeDomain(domain) == "" {
		return ZoneIdentity{}, false
	}
	return ZoneIdentity{
		Project:   p.Project,
		Name:      p.Name,
		Namespace: p.Namespace,
		UID:       types.UID(p.UID),
		Domain:    domain,
	}, true
}

func eventForZone(meter string, zone ZoneIdentity, quantity int64, location string, extra map[string]string, occurredAt time.Time) emission.UsageEvent {
	return emission.UsageEvent{
		Meter:      meter,
		Project:    emission.ProjectRef{Name: zone.Project},
		Source:     SourceURI,
		Quantity:   quantity,
		Dimensions: dimensions(location, extra),
		Resource: &emission.ResourceRef{
			Group:     ResourceGroup,
			Kind:      ResourceKind,
			Namespace: zone.Namespace,
			Name:      zone.Name,
			UID:       zone.UID,
		},
		OccurredAt: occurredAt,
	}
}
