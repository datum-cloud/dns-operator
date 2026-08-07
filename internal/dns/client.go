package dns

import (
	"context"

	"go.miloapis.com/dns-operator/api/v1alpha1"
)

type DNSController interface {
	// // Initialize controller endpoint
	Init() error

	// // Shutdown hook
	Shutdown()

	EnsureZone(ctx context.Context, zone v1alpha1.DNSZone, class v1alpha1.DNSZoneClass) error
	DeleteZone(ctx context.Context, zone v1alpha1.DNSZone) error

	// EnsureRecordSet(
	// 	ctx context.Context,
	// 	zone v1alpha1.DNSZone,
	// 	recordSet v1alpha1.DNSRecordSet,
	// ) error

	// DeleteRecordSet(
	// 	ctx context.Context,
	// 	zone v1alpha1.DNSZone,
	// 	recordSet v1alpha1.DNSRecordSet,
	// ) error

	ReplaceRRSet(
		ctx context.Context,
		zone string,
		recordType string,
		ownerName string,
		ttl int,
		values []string,
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
