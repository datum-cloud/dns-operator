// SPDX-License-Identifier: AGPL-3.0-only

package usage

import "sync"

type queryKey struct {
	domain     string
	rcode      string
	recordType string
}

type counterMap struct {
	mu     sync.Mutex
	counts map[queryKey]int64
}

func newCounterMap() *counterMap {
	return &counterMap{counts: make(map[queryKey]int64)}
}

func (c *counterMap) add(key queryKey, n int64) {
	if n == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = make(map[queryKey]int64)
	}
	c.counts[key] += n
}

// snapshotAndReset returns the current counts and clears the map.
func (c *counterMap) snapshotAndReset() map[queryKey]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.counts
	c.counts = make(map[queryKey]int64)
	if out == nil {
		return map[queryKey]int64{}
	}
	return out
}

func (c *counterMap) restore(counts map[queryKey]int64) {
	if len(counts) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = make(map[queryKey]int64)
	}
	for k, n := range counts {
		c.counts[k] += n
	}
}
