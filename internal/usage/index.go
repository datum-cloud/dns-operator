// SPDX-License-Identifier: AGPL-3.0-only

package usage

import "sync"

// ZoneIndex maps a normalized FQDN to the zone that hosts it.
type ZoneIndex struct {
	mu       sync.RWMutex
	byDomain map[string]ZoneIdentity
}

// Replace atomically swaps the hosted-zone set.
func (z *ZoneIndex) Replace(zones []ZoneIdentity) {
	next := make(map[string]ZoneIdentity, len(zones))
	for _, zone := range zones {
		d := NormalizeDomain(zone.Domain)
		if d == "" {
			continue
		}
		zone.Domain = d
		next[d] = zone
	}
	z.mu.Lock()
	z.byDomain = next
	z.mu.Unlock()
}

// Lookup finds the longest-suffix hosted zone for qname.
func (z *ZoneIndex) Lookup(qname string) (ZoneIdentity, bool) {
	name := NormalizeDomain(qname)
	z.mu.RLock()
	defer z.mu.RUnlock()
	for name != "" {
		if zone, ok := z.byDomain[name]; ok {
			return zone, true
		}
		name = parentDomain(name)
	}
	return ZoneIdentity{}, false
}

func (z *ZoneIndex) get(domain string) (ZoneIdentity, bool) {
	z.mu.RLock()
	defer z.mu.RUnlock()
	zone, ok := z.byDomain[NormalizeDomain(domain)]
	return zone, ok
}
