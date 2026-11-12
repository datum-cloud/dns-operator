// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"slices"
	"testing"

	"go.miloapis.com/dns-operator/internal/internaldns/serving"
)

func TestOnlyResolverAgentsReadTheAckFeed(t *testing.T) {
	names := func(mode serving.Mode) []string {
		feeds := agentFeeds(serving.Config{Region: "east", Shard: "shared-0", Mode: mode})
		out := make([]string, 0, len(feeds))
		for _, feed := range feeds {
			out = append(out, feed.suffix)
		}
		return out
	}
	if got := names(serving.ModeCombined); !slices.Equal(got, []string{"serving", "records"}) {
		t.Fatalf("combined agent feeds = %v, want serving and records only", got)
	}
	if got := names(serving.ModeResolver); !slices.Equal(got, []string{"serving", "records", "acks"}) {
		t.Fatalf("resolver agent feeds = %v, want serving, records and acks", got)
	}
}
