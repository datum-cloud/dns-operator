// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAccessAuthorizationPreservesDeadlinePrecision(t *testing.T) {
	const raw = `{"writerEpoch":1,"sequence":2,"validUntil":"2026-10-08T23:22:29.022357123Z"}`
	var authorization DNSResolverAccessAuthorization
	if err := json.Unmarshal([]byte(raw), &authorization); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		encoded, err := json.Marshal(authorization)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != raw {
			t.Fatalf("authorization deadline changed on round trip: %s", encoded)
		}
		if err := json.Unmarshal(encoded, &authorization); err != nil {
			t.Fatal(err)
		}
	}
	if authorization.ValidUntil.Nanosecond() != 22357123 {
		t.Fatal("authorization lost nanosecond precision")
	}
	// The same instant in another time zone serializes to the same UTC fence.
	authorization.ValidUntil.Time = authorization.ValidUntil.In(time.FixedZone("offset", 3600))
	encoded, err := json.Marshal(authorization)
	if err != nil || string(encoded) != raw {
		t.Fatalf("UTC normalization changed authorization: %s, %v", encoded, err)
	}
	encoded, err = json.Marshal(DNSResolverAccessAuthorization{})
	if err != nil || string(encoded) != `{"writerEpoch":0,"sequence":0,"validUntil":null}` {
		t.Fatalf("zero value encoding changed field names or null deadline: %s, %v", encoded, err)
	}
}
