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

	// EnsureRecordSet writes a record set's records and removes the owner names
	// it wrote and no longer lists. DeleteRecordSet removes the owner names a
	// record set lists or wrote.
	//
	// holders maps an owner name, qualified to the zone, to the record set that
	// still claims it once this one stops (claims.Holders). A name either call
	// would remove that has a holder is written with the holder's records
	// instead, so a name goes only when no record set asks for it.
	//
	// TODO - use pointers for zone and recordset to avoid copying
	EnsureRecordSet(ctx context.Context, zone dnsv1alpha1.DNSZone, recordSet dnsv1alpha1.DNSRecordSet, holders map[string]*dnsv1alpha1.DNSRecordSet) ([]dnsv1alpha1.RecordSetStatus, error)
	DeleteRecordSet(ctx context.Context, zone dnsv1alpha1.DNSZone, recordSet dnsv1alpha1.DNSRecordSet, holders map[string]*dnsv1alpha1.DNSRecordSet) error

	ReplaceRRSet(
		ctx context.Context,
		zone string,
		recordType string,
		ownerName string,
		ttl int,
		values []string,
		ownerRef string,
		observedGeneration int64,
		objectUID string,
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
