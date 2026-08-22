// SPDX-License-Identifier: AGPL-3.0-only

package util

// DefaultTTL is the TTL the DNS backend applies when a record entry carries
// none. It is hardcoded at internal/pdns/client.go:427; DNSZoneClass's
// spec.defaults.defaultTTL is declared but never read by any controller, so
// this is the only default that is real.
const DefaultTTL int64 = 300

// TTLEqual reports whether two TTLs resolve to the same effective value, so a
// nil ("Auto") TTL compares equal to an explicit DefaultTTL.
//
// This matters for round-tripping. A zone file has no way to express "Auto":
// `zone export` writes a $TTL directive and omits the per-record TTL, and
// re-reading resolves that to an explicit DefaultTTL. Comparing the pointers
// naively then reports every Auto record as changed, so export -> apply is
// never idempotent and a drift check built on it alarms forever.
func TTLEqual(a, b *int64) bool {
	return effectiveTTL(a) == effectiveTTL(b)
}

func effectiveTTL(t *int64) int64 {
	if t == nil {
		return DefaultTTL
	}
	return *t
}
