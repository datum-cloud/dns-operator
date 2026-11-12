// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"slices"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/platform"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
)

func prunerFixture() (*consumerPruner, []string, []string, []string) {
	planners := []platform.PlannerConfig{{Region: "region-a", Shard: "shared-0", Members: []model.Member{
		{MemberID: "node-a-0", ReplicaID: "node-a-0", Role: "node"},
		{MemberID: "Regional A/0", ReplicaID: "Regional A/0", Role: "regional"},
	}}}
	return newConsumerPruner(planners),
		[]string{model.ServingSubject("region-a", "shared-0")},
		[]string{model.RecordConsumerFilter("region-a", "shared-0")},
		[]string{ackFilter("region-a", "shared-0")}
}

func TestConsumerPrunerSelectsRemovedMemberFeedsAndMemberAckFeeds(t *testing.T) {
	p, serving, records, acks := prunerFixture()
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	consumers := []transport.ConsumerSummary{
		{Name: "node-a-0-serving", FilterSubjects: serving, LastActive: old},
		{Name: durable("Regional A/0", "records"), FilterSubjects: records, LastActive: old},
		{Name: "control-plane-region-a-shared-0-acks", FilterSubjects: acks, LastActive: old},
		{Name: durable("Regional A/0", "acks"), FilterSubjects: acks, LastActive: old},
		{Name: "front-a-0-serving", FilterSubjects: serving, LastActive: old},
		{Name: "front-a-0-records", FilterSubjects: records, LastActive: old},
		{Name: "front-a-0-acks", FilterSubjects: acks, LastActive: old},
		{Name: "front-a-0-serving-copy", FilterSubjects: serving, LastActive: old},
		{Name: "debug-records", FilterSubjects: serving, LastActive: old},
		{Name: "other-region-serving", FilterSubjects: []string{model.ServingSubject("region-b", "shared-0")}, LastActive: old},
		{Name: "mixed-serving", FilterSubjects: []string{serving[0], records[0]}, LastActive: old},
		{Name: "unfiltered-serving", LastActive: old},
	}
	var got []string
	for _, c := range consumers {
		if p.orphaned(c) {
			got = append(got, c.Name)
		}
	}
	slices.Sort(got)
	want := []string{"front-a-0-acks", "front-a-0-records", "front-a-0-serving", durable("Regional A/0", "acks")}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("orphaned consumers = %v, want %v", got, want)
	}
}

func TestConsumerPrunerWaitsForSustainedIdleness(t *testing.T) {
	p, serving, _, _ := prunerFixture()
	start := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	created := start.Add(-30 * 24 * time.Hour)
	orphan := transport.ConsumerSummary{Name: "front-a-0-serving", FilterSubjects: serving, LastActive: created}

	if got := p.retired([]transport.ConsumerSummary{orphan}, start); len(got) != 0 {
		t.Fatalf("first sighting retired %v; an old creation time alone must not count", got)
	}
	if got := p.retired([]transport.ConsumerSummary{orphan}, start.Add(retiredConsumerGrace-time.Minute)); len(got) != 0 {
		t.Fatalf("retired %v before the grace period passed", got)
	}

	reading := orphan
	reading.Waiting = 1
	p.retired([]transport.ConsumerSummary{reading}, start.Add(retiredConsumerGrace))
	if got := p.retired([]transport.ConsumerSummary{orphan}, start.Add(retiredConsumerGrace+time.Minute)); len(got) != 0 {
		t.Fatalf("retired %v right after a waiting reader reset the clock", got)
	}

	resumed := start.Add(retiredConsumerGrace)
	if got := p.retired([]transport.ConsumerSummary{orphan}, resumed.Add(retiredConsumerGrace)); !slices.Equal(got, []string{orphan.Name}) {
		t.Fatalf("retired = %v, want %s after a full unread grace period", got, orphan.Name)
	}

	active := orphan
	active.LastActive = resumed.Add(retiredConsumerGrace)
	if got := p.retired([]transport.ConsumerSummary{active}, resumed.Add(2*retiredConsumerGrace)); len(got) != 0 {
		t.Fatalf("retired %v although its delivery timestamp moved", got)
	}
}
