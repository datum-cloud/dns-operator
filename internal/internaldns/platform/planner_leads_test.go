// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPlannerLeadsOnlyWhileHoldingItsShard(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 13, 30, 0, 0, time.UTC)
	region, shard, ns := "east", "shared-01", "platform"
	b, _ := json.Marshal(shardState{Holder: "a", Epoch: 4, NextRevision: 9, LeaseUntil: now.Add(time.Minute)})
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "dns-shard-owner-" + model.OpaqueToken(region+"/"+shard), Namespace: ns, UID: "shard-owner-uid"}, Data: map[string]string{"state": string(b)}}
	cl := testClient(t, owner)
	cfg := PlannerConfig{Namespace: ns, Region: region, Shard: shard, LeaseDuration: time.Minute, ClusterBackends: []model.Backend{{MemberID: "cluster-1", Address: "fd00::20", Port: 5300}}}
	planner := func(identity string) *Planner {
		p := &Planner{Client: cl, Allocator: &Allocator{Client: cl, Namespace: ns}, Config: cfg, Now: func() time.Time { return now }}
		p.Config.Identity = identity
		return p
	}
	holder, standby := planner("a"), planner("b")
	if holder.Leads(now) || standby.Leads(now) {
		t.Fatal("a planner led before its first step")
	}
	if err := holder.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if err := standby.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if !holder.Leads(now) {
		t.Fatal("the shard holder does not lead")
	}
	if standby.Leads(now) {
		t.Fatal("a standby leads while another process holds the shard")
	}
	if holder.Leads(now.Add(2 * time.Minute)) {
		t.Fatal("the holder still leads after its lease ran out without a renewal")
	}
}
