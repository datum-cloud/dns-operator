package fake

import (
	"context"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

type FakeDNSClient struct {
	ReplaceCalls []ReplaceCall
	DeleteCalls  []DeleteCall

	ReplaceErr error
	DeleteErr  error

	EnsureZoneCalls []EnsureZoneCall
	DeleteZoneCalls []DeleteZoneCall

	EnsureZoneErr error
	DeleteZoneErr error
}

type EnsureZoneCall struct {
	Zone  string
	Class string
}

type DeleteZoneCall struct {
	Zone string
}

type ReplaceCall struct {
	Zone       string
	RecordType string
	OwnerName  string
	TTL        int
	Values     []string
}

type DeleteCall struct {
	Zone       string
	RecordType string
	OwnerName  string
}

func NewFakeDNSClient() *FakeDNSClient {
	return &FakeDNSClient{
		ReplaceCalls:    []ReplaceCall{},
		DeleteCalls:     []DeleteCall{},
		EnsureZoneCalls: []EnsureZoneCall{},
		DeleteZoneCalls: []DeleteZoneCall{},
	}
}

func (f *FakeDNSClient) Init() error {
	return nil
}

func (f *FakeDNSClient) Shutdown() {}

func (f *FakeDNSClient) GetZoneNameservers(_ context.Context, zone dnsv1alpha1.DNSZone, class dnsv1alpha1.DNSZoneClass) []string {
	return []string{}
}

func (f *FakeDNSClient) EnsureZone(_ context.Context, z dnsv1alpha1.DNSZone, c dnsv1alpha1.DNSZoneClass) error {
	f.EnsureZoneCalls = append(f.EnsureZoneCalls, EnsureZoneCall{
		Zone:  z.Name,
		Class: c.Name,
	})

	return f.EnsureZoneErr
}

func (f *FakeDNSClient) DeleteZone(_ context.Context, z dnsv1alpha1.DNSZone) error {
	f.DeleteZoneCalls = append(f.DeleteZoneCalls, DeleteZoneCall{
		Zone: z.Name,
	})
	return f.DeleteZoneErr
}

func (c *FakeDNSClient) EnsureRecordSet(ctx context.Context, zone dnsv1alpha1.DNSZone, recordSet dnsv1alpha1.DNSRecordSet) (error, []dnsv1alpha1.RecordSetStatus) {
	return nil, nil
}

func (c *FakeDNSClient) DeleteRecordSet(ctx context.Context, zone dnsv1alpha1.DNSZone, recordSet dnsv1alpha1.DNSRecordSet) error {
	return nil
}

func (f *FakeDNSClient) ReplaceRRSet(
	_ context.Context,
	zone, recordType, ownerName string,
	ttl int,
	values []string,
	dnsZoneRef string,
	observedGeneration int64,
) error {
	f.ReplaceCalls = append(f.ReplaceCalls, ReplaceCall{
		Zone:       zone,
		RecordType: recordType,
		OwnerName:  ownerName,
		TTL:        ttl,
		Values:     values,
	})
	return f.ReplaceErr
}

func (f *FakeDNSClient) DeleteRRSet(
	_ context.Context,
	zone, recordType, ownerName string,
) error {
	f.DeleteCalls = append(f.DeleteCalls, DeleteCall{
		Zone:       zone,
		RecordType: recordType,
		OwnerName:  ownerName,
	})
	return f.DeleteErr
}
