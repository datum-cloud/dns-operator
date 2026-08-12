package dns

import (
	"context"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

type DNSController interface {
	// // Initialize controller endpoint
	Init() error

	// // Shutdown hook
	Shutdown()

	EnsureZone(ctx context.Context, zone dnsv1alpha1.DNSZone, class dnsv1alpha1.DNSZoneClass) error
	DeleteZone(ctx context.Context, zone dnsv1alpha1.DNSZone) error

	GetZoneNameservers(ctx context.Context, zone dnsv1alpha1.DNSZone, class dnsv1alpha1.DNSZoneClass) []string

	// TODO - use pointers for zone and recordset to avoid copying
	EnsureRecordSet(ctx context.Context, zone dnsv1alpha1.DNSZone, recordSet dnsv1alpha1.DNSRecordSet) (error, []dnsv1alpha1.RecordSetStatus)
	DeleteRecordSet(ctx context.Context, zone dnsv1alpha1.DNSZone, recordSet dnsv1alpha1.DNSRecordSet) error

	ReplaceRRSet(
		ctx context.Context,
		zone string,
		recordType string,
		ownerName string,
		ttl int,
		values []string,
		dnsZoneRef string,
		observedGeneration int64,
	) error

	DeleteRRSet(
		ctx context.Context,
		zone, recordType, ownerName string,
	) error
}

type DNSClient struct {
	Name string
	Type string
	DNSController
}
