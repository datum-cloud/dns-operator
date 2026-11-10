// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/platform"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
)

const (
	// consumerPruneCheck keeps the loop's success fresh for liveness while the
	// stream is listed only every consumerPruneInterval.
	consumerPruneCheck    = 30 * time.Second
	consumerPruneInterval = 10 * time.Minute
	// retiredConsumerGrace is how long a consumer must stay continuously
	// unread, as observed by this leader, before it is deleted. Broker restarts
	// reset a consumer's delivery timestamps, so they alone cannot prove that.
	retiredConsumerGrace = 6 * time.Hour
)

// memberFeedFilter maps each member feed to the subject filter its durable reads.
var memberFeedFilter = map[string]func(region, shard string) string{
	"serving": model.ServingSubject,
	"records": model.RecordConsumerFilter,
	ackFeed:   ackFilter,
}

// consumerPruner deletes the durable consumers that no member reads, so they
// stop reporting as unread.
type consumerPruner struct {
	planners []platform.PlannerConfig
	mu       sync.Mutex
	last     time.Time
	idle     map[string]idleConsumer
}

type idleConsumer struct {
	since      time.Time
	lastActive time.Time
}

func newConsumerPruner(planners []platform.PlannerConfig) *consumerPruner {
	return &consumerPruner{planners: planners, idle: map[string]idleConsumer{}}
}

func (p *consumerPruner) step(bus *transport.JetStream, stream string) func(context.Context) error {
	return func(ctx context.Context) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		now := time.Now()
		if now.Sub(p.last) < consumerPruneInterval {
			return nil
		}
		// A gap means another replica led meanwhile; idleness this pruner did
		// not watch cannot count toward the grace period.
		if !p.last.IsZero() && now.Sub(p.last) > 2*consumerPruneInterval {
			p.idle = map[string]idleConsumer{}
		}
		p.last = now
		consumers, err := bus.ListConsumers(ctx, stream)
		if err != nil {
			return err
		}
		var errs error
		for _, name := range p.retired(consumers, now) {
			if err := bus.DeleteConsumer(ctx, stream, name); err != nil {
				errs = errors.Join(errs, err)
				continue
			}
			delete(p.idle, name)
		}
		return errs
	}
}

// retired names the durable consumers that have read a shard this control
// plane plans, under a member feed's name and filter, for no configured member,
// and have had no reader for the whole grace period this pruner has watched.
func (p *consumerPruner) retired(consumers []transport.ConsumerSummary, now time.Time) []string {
	candidates := map[string]transport.ConsumerSummary{}
	for _, c := range consumers {
		if p.orphaned(c) {
			candidates[c.Name] = c
		}
	}
	var retired []string
	for name := range p.idle {
		if _, ok := candidates[name]; !ok {
			delete(p.idle, name)
		}
	}
	for name, c := range candidates {
		seen, ok := p.idle[name]
		if c.Waiting > 0 || (ok && !c.LastActive.Equal(seen.lastActive)) {
			p.idle[name] = idleConsumer{since: now, lastActive: c.LastActive}
			continue
		}
		if !ok {
			p.idle[name] = idleConsumer{since: now, lastActive: c.LastActive}
			continue
		}
		if now.Sub(seen.since) >= retiredConsumerGrace && now.Sub(c.LastActive) >= retiredConsumerGrace {
			retired = append(retired, name)
		}
	}
	return retired
}

// orphaned reports a consumer named and filtered like a member feed of a shard
// this control plane plans, whose member is not configured for that shard. A
// member's ack feed is always a candidate: members that verify their own
// publications leave it unread, and the grace period spares one that reads it.
func (p *consumerPruner) orphaned(c transport.ConsumerSummary) bool {
	if len(c.FilterSubjects) != 1 {
		return false
	}
	for feed, filterFor := range memberFeedFilter {
		if !strings.HasSuffix(c.Name, "-"+feed) {
			continue
		}
		for _, planner := range p.planners {
			if c.FilterSubjects[0] != filterFor(planner.Region, planner.Shard) {
				continue
			}
			if c.Name == durable(roleControlPlane, planner.Region, planner.Shard, ackFeed) {
				return false
			}
			if feed == ackFeed {
				return true
			}
			for _, m := range planner.Members {
				if c.Name == durable(m.MemberID, feed) {
					return false
				}
			}
			return true
		}
	}
	return false
}
