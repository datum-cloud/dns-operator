// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"errors"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/platform"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
)

const (
	consumerPruneInterval = 10 * time.Minute
	// retiredConsumerGrace leaves a member's consumers alone while it may still
	// be starting up ahead of the configuration that names it.
	retiredConsumerGrace = 6 * time.Hour
)

var memberFeeds = []string{"serving", "records", "acks"}

// retiredConsumers names the durable consumers that read a shard this control
// plane plans but belong to no member configured for it. A consumer with a
// waiting reader, or with any activity inside the grace period, is kept.
func retiredConsumers(planners []platform.PlannerConfig, consumers []transport.ConsumerSummary, now time.Time) []string {
	shardOf := map[string]string{}
	expected := map[string]map[string]bool{}
	for _, p := range planners {
		key := p.Region + "/" + p.Shard
		for _, filter := range []string{model.ServingSubject(p.Region, p.Shard), model.RecordConsumerFilter(p.Region, p.Shard), ackFilter(p.Region, p.Shard)} {
			shardOf[filter] = key
		}
		names := map[string]bool{durable(roleControlPlane, p.Region, p.Shard, "acks"): true}
		for _, m := range p.Members {
			for _, feed := range memberFeeds {
				names[durable(m.MemberID, feed)] = true
			}
		}
		expected[key] = names
	}
	var retired []string
	for _, c := range consumers {
		if len(c.FilterSubjects) == 0 || c.Waiting > 0 || now.Sub(c.LastActive) < retiredConsumerGrace {
			continue
		}
		key := ""
		for _, filter := range c.FilterSubjects {
			k, ok := shardOf[filter]
			if !ok || (key != "" && k != key) {
				key = ""
				break
			}
			key = k
		}
		if key == "" || expected[key][c.Name] {
			continue
		}
		retired = append(retired, c.Name)
	}
	return retired
}

// pruneRetiredConsumers deletes the stream consumers left behind by members
// removed from this control plane's configuration, so they stop reporting as
// unread.
func pruneRetiredConsumers(bus *transport.JetStream, stream string, planners []platform.PlannerConfig) func(context.Context) error {
	return func(ctx context.Context) error {
		consumers, err := bus.ListConsumers(ctx, stream)
		if err != nil {
			return err
		}
		var errs error
		for _, name := range retiredConsumers(planners, consumers, time.Now()) {
			errs = errors.Join(errs, bus.DeleteConsumer(ctx, stream, name))
		}
		return errs
	}
}
