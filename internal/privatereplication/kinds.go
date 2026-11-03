// SPDX-License-Identifier: AGPL-3.0-only

// Package privatereplication carries private DNS objects between project
// control planes and a federation hub, so that each region's control plane can
// read its inputs from its own cluster and keep publishing while the project
// API is unreachable.
//
// The replicator copies a project's private DNS objects into the hub under
// the project's mapped namespace. The hub places them in every region. The
// mirror carries the other way: each region's status entries, merged by
// region, and objects that a region wrote itself.
package privatereplication

import (
	"go.miloapis.com/dns-operator/internal/internaldns/projectstatus"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	group   = "dns.networking.miloapis.com"
	version = "v1alpha1"

	// OriginLabel names where an object was written: "project" for the
	// project API, or a region's name for an object written in that region.
	// No path writes an object back to its origin.
	OriginLabel   = "internal-dns.miloapis.com/origin"
	OriginProject = "project"

	// UpstreamClusterAnnotation holds the project's cluster name, as the
	// project discovery names it, on a mapped namespace in the hub.
	UpstreamClusterAnnotation = "internal-dns.miloapis.com/upstream-cluster"
	// ProjectUIDAnnotation holds the project's UID on a mapped namespace, so
	// a regional control plane keeps the identity it publishes under.
	ProjectUIDAnnotation = "internal-dns.miloapis.com/project-uid"
	// UpstreamNamespaceAnnotation holds the project namespace a mapped
	// namespace stands for.
	UpstreamNamespaceAnnotation = "meta.datumapis.com/upstream-namespace"
	// ReplicatedLabel marks every namespace and object the replicator or a
	// region places in the hub, so one propagation policy can select them.
	ReplicatedLabel = "internal-dns.miloapis.com/replicated"

	// OriginGenerationAnnotation is the generation of the object at its
	// origin. A copy replaces another only when it is newer.
	OriginGenerationAnnotation = projectstatus.OriginGenerationAnnotation

	// OriginUIDAnnotation is the object's UID at its origin. References
	// between objects carry origin UIDs, so a region reads copies by them.
	OriginUIDAnnotation = "internal-dns.miloapis.com/origin-uid"

	finalizer = "internal-dns.miloapis.com/replicate"
)

// Kinds are the project kinds a regional control plane reads.
var Kinds = []schema.GroupVersionKind{
	gvk("DNSZone"),
	gvk("DNSRecordSet"),
	gvk("DNSZoneAssociation"),
	gvk("DNSNamingPolicy"),
	gvk("DNSRegistration"),
	gvk("DNSRecordContribution"),
	gvk("DNSResolverContext"),
	gvk("DNSResolverAccessBinding"),
}

// RegionWritable are the kinds a region may write itself and send to the
// other regions and to the project API.
var RegionWritable = []schema.GroupVersionKind{
	gvk("DNSRegistration"),
	gvk("DNSRecordContribution"),
}

func gvk(kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}

// MappedNamespace is the hub namespace that stands for a project namespace.
func MappedNamespace(upstreamNamespaceUID string) string {
	return "ns-" + upstreamNamespaceUID
}
