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

func TestRetiredConsumersPrunesOnlyIdleConsumersOfRemovedMembers(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	idle := now.Add(-2 * retiredConsumerGrace)
	planners := []platform.PlannerConfig{{Region: "region-a", Shard: "shared-0", Members: []model.Member{
		{MemberID: "node-a-0", ReplicaID: "node-a-0", Role: "node"},
		{MemberID: "regional-a-0", ReplicaID: "regional-a-0", Role: "regional"},
	}}}
	serving := []string{model.ServingSubject("region-a", "shared-0")}
	records := []string{model.RecordConsumerFilter("region-a", "shared-0")}
	acks := []string{ackFilter("region-a", "shared-0")}
	consumers := []transport.ConsumerSummary{
		{Name: "node-a-0-serving", FilterSubjects: serving, LastActive: idle},
		{Name: "regional-a-0-records", FilterSubjects: records, LastActive: idle},
		{Name: "control-plane-region-a-shared-0-acks", FilterSubjects: acks, LastActive: idle},
		{Name: "front-a-0-serving", FilterSubjects: serving, LastActive: idle},
		{Name: "front-a-0-records", FilterSubjects: records, LastActive: idle},
		{Name: "front-a-0-acks", FilterSubjects: acks, LastActive: idle},
		{Name: "starting-a-0-serving", FilterSubjects: serving, LastActive: now.Add(-time.Minute)},
		{Name: "reading-a-0-serving", FilterSubjects: serving, LastActive: idle, Waiting: 1},
		{Name: "other-region-serving", FilterSubjects: []string{model.ServingSubject("region-b", "shared-0")}, LastActive: idle},
		{Name: "mixed-filters", FilterSubjects: []string{serving[0], model.ServingSubject("region-b", "shared-0")}, LastActive: idle},
		{Name: "unfiltered", LastActive: idle},
	}
	got := retiredConsumers(planners, consumers, now)
	slices.Sort(got)
	want := []string{"front-a-0-acks", "front-a-0-records", "front-a-0-serving"}
	if !slices.Equal(got, want) {
		t.Fatalf("retired consumers = %v, want %v", got, want)
	}
}
