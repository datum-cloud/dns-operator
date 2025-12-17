package discovery

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/miekg/dns"
	"github.com/projectdiscovery/dnsx/libs/dnsx"
	"github.com/stretchr/testify/require"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

func TestDiscoverZoneRecordsContinuesAfterTimeout(t *testing.T) {
	t.Cleanup(func() {
		queryRecordsFunc = queryRecordsForName
	})

	stub := newStubRecordLookup()
	stub.responses["example.com"] = map[uint16][]dns.RR{
		dns.TypeA: {mustRR(t, "example.com. 60 IN A 1.2.3.4")},
	}
	stub.errors["www.example.com"] = context.DeadlineExceeded

	queryRecordsFunc = stub.query

	results, err := DiscoverZoneRecords(context.Background(), "example.com")
	require.NoError(t, err)

	rs := findRecordSet(results, dnsv1alpha1.RRTypeA)
	require.NotNil(t, rs, "expected A recordset to be present")
	require.Len(t, rs.Records, 1)
	entry := rs.Records[0]
	require.Equal(t, "@", entry.Name)
	require.NotNil(t, entry.A)
	require.Equal(t, "1.2.3.4", entry.A.Content)
}

func TestDiscoverZoneRecordsFailsWhenApexLookupErrors(t *testing.T) {
	t.Cleanup(func() {
		queryRecordsFunc = queryRecordsForName
	})

	stub := newStubRecordLookup()
	stub.errors["example.com"] = errors.New("dns failure")
	queryRecordsFunc = stub.query

	_, err := DiscoverZoneRecords(context.Background(), "example.com")
	require.Error(t, err)
	require.ErrorContains(t, err, "dns failure")
}

func TestDiscoverZoneRecordsAllowsApexTimeoutWithOtherRecords(t *testing.T) {
	t.Cleanup(func() {
		queryRecordsFunc = queryRecordsForName
	})

	stub := newStubRecordLookup()
	stub.errors["example.com"] = context.DeadlineExceeded
	stub.responses["www.example.com"] = map[uint16][]dns.RR{
		dns.TypeA: {mustRR(t, "www.example.com. 30 IN A 5.6.7.8")},
	}
	queryRecordsFunc = stub.query

	results, err := DiscoverZoneRecords(context.Background(), "example.com")
	require.NoError(t, err)

	rs := findRecordSet(results, dnsv1alpha1.RRTypeA)
	require.NotNil(t, rs)
	entry := findRecordByName(rs.Records, "www")
	require.NotNil(t, entry, "expected www entry when apex times out")
	require.NotNil(t, entry.A)
	require.Equal(t, "5.6.7.8", entry.A.Content)
}

type stubRecordLookup struct {
	mu        sync.Mutex
	responses map[string]map[uint16][]dns.RR
	errors    map[string]error
	calls     []string
}

func newStubRecordLookup() *stubRecordLookup {
	return &stubRecordLookup{
		responses: make(map[string]map[uint16][]dns.RR),
		errors:    make(map[string]error),
	}
}

func (s *stubRecordLookup) query(_ context.Context, name string, _ *dnsx.DNSX, _ []uint16) (map[uint16][]dns.RR, error) {
	s.mu.Lock()
	s.calls = append(s.calls, name)
	resp, respOK := s.responses[name]
	err, errOK := s.errors[name]
	s.mu.Unlock()

	if errOK {
		return nil, err
	}
	if respOK {
		return resp, nil
	}
	return nil, nil
}

func mustRR(t *testing.T, rr string) dns.RR {
	t.Helper()
	record, err := dns.NewRR(rr)
	require.NoError(t, err)
	return record
}

func findRecordSet(sets []dnsv1alpha1.DiscoveredRecordSet, rt dnsv1alpha1.RRType) *dnsv1alpha1.DiscoveredRecordSet {
	for i := range sets {
		if sets[i].RecordType == rt {
			return &sets[i]
		}
	}
	return nil
}

func findRecordByName(entries []dnsv1alpha1.RecordEntry, name string) *dnsv1alpha1.RecordEntry {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}
