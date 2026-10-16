package v1alpha1

import (
	"encoding/json"
	"testing"
)

func TestResolverAccessHasNoExpiryOrClusterIdentity(t *testing.T) {
	a := DNSResolverAccessVersion{Epoch: 1, Sequence: 2}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"epoch":1,"sequence":2}` {
		t.Fatalf("unexpected access fence: %s", b)
	}
}
