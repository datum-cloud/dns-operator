// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"strings"
	"testing"

	"go.miloapis.com/dns-operator/internal/internaldns/platform"
)

func TestRegionalPrefixes(t *testing.T) {
	shard := func(region, name, prefix string) platform.PlannerConfig {
		return platform.PlannerConfig{Region: region, Shard: name, ClusterPrefix: prefix}
	}
	for _, tc := range []struct {
		name   string
		shards []platform.PlannerConfig
		want   map[string]string
		err    string
	}{
		{name: "shared only", shards: []platform.PlannerConfig{shard("east", "a", "")}, want: map[string]string{}},
		{name: "own pool", shards: []platform.PlannerConfig{shard("east", "a", ""), shard("west", "a", "fd70:300::/96")},
			want: map[string]string{"west": "fd70:300::/96"}},
		{name: "conflicting shards", shards: []platform.PlannerConfig{shard("west", "a", "fd70:300::/96"), shard("west", "b", "fd70:400::/96")},
			err: "conflicting clusterPrefix"},
		{name: "partial region", shards: []platform.PlannerConfig{shard("west", "a", "fd70:300::/96"), shard("west", "b", "")},
			err: "some shards but not others"},
		{name: "overlaps shared", shards: []platform.PlannerConfig{shard("west", "a", "fd70:200::/112")}, err: "overlaps the shared"},
		{name: "overlaps another region", shards: []platform.PlannerConfig{shard("west", "a", "fd70:300::/96"), shard("north", "a", "fd70:300::/100")},
			err: "overlaps region"},
		{name: "too small", shards: []platform.PlannerConfig{shard("west", "a", "fd70:300::/120")}, err: "16 host bits"},
	} {
		c := Config{ClusterPrefix: "fd70:200::/96", Shards: tc.shards}
		got, err := c.regionalPrefixes()
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: want error containing %q, got %v", tc.name, tc.err, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		}
	}
}
