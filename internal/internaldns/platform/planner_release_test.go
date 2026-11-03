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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReleasedShardLeaseIsTakenOverAtOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 1, 45, 0, 0, time.UTC)
	region, shard, ns := "east", "shared-01", "platform"
	b, _ := json.Marshal(shardState{Holder: "old", Epoch: 4, NextRevision: 9, LeaseUntil: now.Add(2 * time.Minute)})
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "dns-shard-owner-" + model.OpaqueToken(region+"/"+shard), Namespace: ns, UID: "shard-owner-uid"}, Data: map[string]string{"state": string(b)}}
	cl := testClient(t, owner)
	cfg := PlannerConfig{Namespace: ns, Region: region, Shard: shard, LeaseDuration: time.Minute, ClusterBackends: []model.Backend{{MemberID: "cluster-1", Address: "fd00::20", Port: 5300}}}

	stopping := true
	old := Planner{Client: cl, Allocator: &Allocator{Client: cl, Namespace: ns}, Config: cfg, Now: func() time.Time { return now }, Stopping: func() bool { return stopping }}
	old.Config.Identity = "old"
	if err := old.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := old.Release(ctx, now); err != nil || !ok {
		t.Fatalf("old planner did not release its lease: %v %v", ok, err)
	}
	if ok, err := old.Release(ctx, now); err != nil || ok {
		t.Fatalf("a second release changed an already released lease: %v %v", ok, err)
	}

	next := Planner{Client: cl, Allocator: &Allocator{Client: cl, Namespace: ns}, Config: cfg, Now: func() time.Time { return now.Add(time.Second) }}
	next.Config.Identity = "new"
	if err := next.Step(ctx); err != nil {
		t.Fatal(err)
	}
	var current corev1.ConfigMap
	if err := cl.Get(ctx, client.ObjectKeyFromObject(owner), &current); err != nil {
		t.Fatal(err)
	}
	var state shardState
	if err := json.Unmarshal([]byte(current.Data["state"]), &state); err != nil {
		t.Fatal(err)
	}
	if state.Holder != "new" || state.Epoch <= 4 {
		t.Fatalf("new planner did not take the released shard over: %+v", state)
	}
}
