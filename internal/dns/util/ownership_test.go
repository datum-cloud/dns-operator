// SPDX-License-Identifier: AGPL-3.0-only

package util

import "testing"

func TestMachineOwned(t *testing.T) {
	tests := []struct {
		name       string
		labels     map[string]string
		wantOwned  bool
		wantSource string
	}{
		{
			name:      "nil labels",
			labels:    nil,
			wantOwned: false,
		},
		{
			name:      "empty labels",
			labels:    map[string]string{},
			wantOwned: false,
		},
		{
			name:      "an ordinary user record",
			labels:    map[string]string{"app": "mine"},
			wantOwned: false,
		},
		// Each of the three markers alone is sufficient. This is the whole
		// point: the producer's GC does not select on source-kind, so a rule
		// resting on it alone would fail open the day it stopped being set.
		{
			name:      "source-kind alone",
			labels:    map[string]string{LabelSourceKind: "Gateway"},
			wantOwned: true,
		},
		{
			name:      "managed alone",
			labels:    map[string]string{LabelDNSManaged: "true"},
			wantOwned: true,
		},
		{
			name:      "managed-by alone",
			labels:    map[string]string{LabelManagedBy: "networking.datumapis.com"},
			wantOwned: true,
		},
		{
			// The case that motivated consolidating this: a set the producer
			// labelled, minus source-kind. The narrower copy in the bulk path
			// missed exactly this and let an import write a change the
			// controller would revert.
			name: "labelled but missing source-kind is still owned",
			labels: map[string]string{
				LabelManagedBy:       "networking.datumapis.com",
				LabelDNSManaged:      "true",
				LabelSourceName:      "web",
				LabelSourceNamespace: "shop",
			},
			wantOwned:  true,
			wantSource: "shop/web",
		},
		{
			name: "all five, as the producer writes them",
			labels: map[string]string{
				LabelManagedBy:       "networking.datumapis.com",
				LabelDNSManaged:      "true",
				LabelSourceKind:      "Gateway",
				LabelSourceName:      "web",
				LabelSourceNamespace: "shop",
			},
			wantOwned:  true,
			wantSource: "shop/web",
		},
		{
			name: "name without namespace",
			labels: map[string]string{
				LabelSourceKind: "Gateway",
				LabelSourceName: "web",
			},
			wantOwned:  true,
			wantSource: "web",
		},
		{
			name:       "owned but unnamed",
			labels:     map[string]string{LabelSourceKind: "Gateway"},
			wantOwned:  true,
			wantSource: "",
		},
		{
			// Label values are matched case-insensitively, as both the previous
			// implementations did.
			name:       "case does not matter",
			labels:     map[string]string{LabelSourceKind: "gateway", LabelSourceName: "web"},
			wantOwned:  true,
			wantSource: "web",
		},
		{
			name:      "managed set to false is not owned",
			labels:    map[string]string{LabelDNSManaged: "false"},
			wantOwned: false,
		},
		{
			name:      "another controller's managed-by is not ours",
			labels:    map[string]string{LabelManagedBy: "someone.else.example"},
			wantOwned: false,
		},
		{
			// A source name on an otherwise unowned object must not make it
			// owned, or an unrelated label would grant immunity from editing.
			name:      "a source name alone is not ownership",
			labels:    map[string]string{LabelSourceName: "web"},
			wantOwned: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			owned, source := MachineOwned(tc.labels)
			if owned != tc.wantOwned {
				t.Errorf("owned = %v, want %v", owned, tc.wantOwned)
			}
			if source != tc.wantSource {
				t.Errorf("source = %q, want %q", source, tc.wantSource)
			}
		})
	}
}

// An unowned set must never report a source, or a caller rendering "managed by
// X" would name an owner for a record anyone may edit.
func TestMachineOwnedNeverNamesAnUnownedSource(t *testing.T) {
	_, source := MachineOwned(map[string]string{
		LabelSourceName:      "web",
		LabelSourceNamespace: "shop",
	})
	if source != "" {
		t.Errorf("source = %q for an unowned set, want empty", source)
	}
}
