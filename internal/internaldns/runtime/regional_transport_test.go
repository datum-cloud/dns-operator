// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"sort"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/platform"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
)

type capturePublisher struct{ regions []string }

func (p *capturePublisher) Publish(_ context.Context, _ string, e model.Envelope) (transport.PublishAck, error) {
	p.regions = append(p.regions, e.Region)
	return transport.PublishAck{}, nil
}

func TestRegionalWorkersStartIndependently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, 2)
	terminal := make(chan error, 2)
	planners := []platform.PlannerConfig{{Region: "east", Shard: "one"}, {Region: "west", Shard: "two"}}
	startRegionalWorkers(planners, func(region string, _ []platform.PlannerConfig) error {
		started <- region
		<-ctx.Done()
		return nil
	}, terminal)
	var regions []string
	deadline := time.After(time.Second)
	for len(regions) < 2 {
		select {
		case region := <-started:
			regions = append(regions, region)
		case <-deadline:
			t.Fatalf("a blocked region prevented another worker from starting: %v", regions)
		}
	}
	sort.Strings(regions)
	if regions[0] != "east" || regions[1] != "west" {
		t.Fatalf("unexpected regional workers: %v", regions)
	}
}

type captureBootstrap struct{ regions []string }

func (b *captureBootstrap) Put(_ context.Context, _ string, e model.Envelope) (uint64, error) {
	b.regions = append(b.regions, e.Region)
	return 1, nil
}

func TestRegionalTransportRoutesWithoutCrossRegionFallback(t *testing.T) {
	eastPub, westPub := &capturePublisher{}, &capturePublisher{}
	eastStore, westStore := &captureBootstrap{}, &captureBootstrap{}
	r := &regionalTransport{routes: map[string]regionalRoute{"east": {publisher: eastPub, bootstrap: eastStore}, "west": {publisher: westPub, bootstrap: westStore}}}
	for _, region := range []string{"east", "west"} {
		env, err := model.NewEnvelope(model.KindServingSnapshot, "event-"+region, region, "one", "resource", 1, 1, time.Now(), map[string]string{"region": region})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Publish(context.Background(), "subject", env); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Put(context.Background(), "key", env); err != nil {
			t.Fatal(err)
		}
	}
	if len(eastPub.regions) != 1 || eastPub.regions[0] != "east" || len(westPub.regions) != 1 || westPub.regions[0] != "west" {
		t.Fatalf("publish crossed regions: east=%v west=%v", eastPub.regions, westPub.regions)
	}
	if len(eastStore.regions) != 1 || len(westStore.regions) != 1 {
		t.Fatalf("bootstrap crossed regions: east=%v west=%v", eastStore.regions, westStore.regions)
	}
	unknown, _ := model.NewEnvelope(model.KindServingSnapshot, "event-unknown", "north", "one", "resource", 1, 1, time.Now(), map[string]string{"region": "north"})
	if _, err := r.Publish(context.Background(), "subject", unknown); err == nil {
		t.Fatal("unknown region silently fell back to another transport")
	}
}
