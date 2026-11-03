# Cached Private DNS zone inputs

The control plane can compile a zone from its existing watch cache instead of
listing the zone's inputs from the project API on every change. To enable this
behavior, set both `perZoneReconcile` and `cachedZoneInputs` in the runtime
configuration. Both options are off by default.

The compiler still evaluates the complete zone and publishes a complete
snapshot.
The project reconciliation pass is unchanged. Cached inputs do not implement
[incremental chunk
delivery](../../enhancements/private-dns-incremental-zones.md#phase-2-content-addressed-chunks).

## Cached and live reads

The cache indexes record sets and registrations by zone, and contributions by
registration name. The compiler receives its input reader separately from its
source API client. This separation allows another input source without changing
the compilation rules; it does not create regional copies of project objects.

The compiler still reads associations, resolver contexts, reference pins, and
resolver bindings from the API. Before publishing, it reads the access inputs
again and retries if they changed.

## Consistency checks

Each writer epoch starts with a successful compilation from live inputs. Before
the compiler uses the cache, the cache must contain the same object lifetimes as
that baseline, with matching or newer generations and matching deletion state.
Objects absent from the baseline must also disappear from the cache. The
compiler
does not order resource versions because the API treats them as opaque values.
If the cache cannot catch up, the compiler continues to use live inputs.

The previous publication retains registration and contribution versions, called
*fences*, that prevent older input from retiring published data. The compiler
checks cached inputs against those versions:

- For a missing contribution, it reads the source object by name to confirm the
  deletion.
- For a missing or outdated registration, it compiles the zone from live inputs.
- If a cached result loses any previously accepted input, it compiles the zone
  from live inputs before publishing. This check covers dependencies across
  record kinds.

Status writes read the current object and use an optimistic lock, so a stale
copy cannot overwrite a concurrent update.

On the first cached compilation after each five-minute comparison interval, the
compiler also builds a plan from live inputs. It publishes the live result and
increments `internal_dns_cached_plan_mismatches_total` if the plans differ.
After
any fallback to live inputs, the cache must catch up with the new baseline.

The control plane also caches the previous plan's chunks by active manifest,
with one entry per zone. This cache avoids rereading unchanged publication data.

## Rollback and monitoring

To stop reading zone inputs from the cache, disable `cachedZoneInputs`. The
publication format does not change, and per-zone reconciliation can remain
enabled.

Monitor API request rates, publication latency, and
`internal_dns_cached_plan_mismatches_total`. Cache reads replace input lists;
access checks and changed status writes still require live requests.
