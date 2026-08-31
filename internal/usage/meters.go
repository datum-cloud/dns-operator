// SPDX-License-Identifier: AGPL-3.0-only

// Package usage emits billed DNS usage events against the published DNS meters.
package usage

// Meter names must match config/components/service-catalog/services_v1alpha1_serviceconfiguration_dns.yaml.
const (
	MeterZoneQueries   = "dns.networking.miloapis.com/zone/queries"
	MeterZones         = "dns.networking.miloapis.com/zones"
	MeterRecordsActive = "dns.networking.miloapis.com/records/active"

	SourceURI = "//dns.networking.miloapis.com/controllers/usage-reporter"

	// MetadataKind is the PowerDNS domain-metadata kind that carries
	// MarshalIdentity JSON. LightningStream replicates it with the LMDB
	// so edge pods can attribute queries without DNSZone CRs.
	MetadataKind = "DATUM-USAGE"

	DimRcode      = "rcode"
	DimRecordType = "record_type"
	DimLocation   = "location"

	ResourceGroup = "dns.networking.miloapis.com"
	ResourceKind  = "DNSZone"
)
