package v1alpha1

import (
	"encoding/json"
	"testing"
)

func TestResolverAccessHasNoExpiryOrClusterIdentity(t *testing.T) {
	a := DNSResolverAccessAuthorization{WriterEpoch: 1, Sequence: 2}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"writerEpoch":1,"sequence":2}` {
		t.Fatalf("unexpected access fence: %s", b)
	}
	b, err = json.Marshal(DNSProducerPrincipal{Subject: "system:serviceaccount:compute:dns-publisher"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"subject":"system:serviceaccount:compute:dns-publisher"}` {
		t.Fatalf("unexpected publisher identity: %s", b)
	}
}
