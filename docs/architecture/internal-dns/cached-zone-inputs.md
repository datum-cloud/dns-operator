# Cached Private DNS zone inputs

The control plane can compile a zone from its existing watch cache. Enable both
`perZoneReconcile` and `cachedZoneInputs` in the runtime configuration. Both options
are off by default. The project pass and the full-zone compiler are unchanged.

Record sets and registrations are indexed by zone; contributions are indexed by
registration name. The reader is supplied independently of the source API client,
so the same path can watch project objects or regional copies. Associations,
resolver contexts, reference pins and resolver bindings remain live reads.

Each writer epoch begins with a successful live compile. Cached inputs qualify
only when their UIDs and generations have caught up with the live baseline and
extra objects have disappeared. Resource versions are opaque and are never ordered.
A cache that cannot catch up keeps the zone on live reads.

Registration and contribution fences prevent stale generations from retiring
published data. Missing contributions are confirmed by a live read of their source
name. Missing or lagging registrations trigger a full live compile. Losing any
previously accepted input also triggers a full live compile, covering dependencies
across record kinds. Status writes read the current object and use an optimistic
lock. Before publication, the control plane checks the live access snapshot again.

Every five minutes, a cached compile is compared with a live compile. The live
result is used, and differences increment
`internal_dns_cached_plan_mismatches_total`. After a fallback, the cache must catch
up with the new live baseline. Previous-plan chunks are cached by active manifest,
with one entry per zone.

To roll back, disable `cachedZoneInputs`. The publication format is unchanged.
Watch API request rates as well as latency: cache reads replace namespace input
lists, while access checks and changed status writes still require live requests.
