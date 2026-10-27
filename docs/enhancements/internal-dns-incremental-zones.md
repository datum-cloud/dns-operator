# Internal DNS incremental zones

This design reduces the work required to publish a record change. Phase 1,
cached zone inputs, is implemented behind configuration options. Phase 2,
content-addressed chunks, remains a proposal. Independent regional writers also
remain a separate proposal.

## Why this exists

Every change to a private zone does whole-zone work three times:

- Without cached inputs, the control plane lists the project's namespace inputs.
  With or without cached inputs, it compiles the complete zone.
- It writes the whole compiled zone as new chunks and sends every chunk to
  every serving member in each region the zone is published to.
- Each member verifies the complete payload, saves a checkpoint, and activates
  a replacement CoreDNS context snapshot.

Compilation took less than 5 ms for the zones in the initial test deployment.
Listing, writing, sending, and loading data grow with the zone and project size.
Repeated input lists doubled the control plane's API requests when that
deployment first enabled per-zone reconciliation.

## Goals

- Record, registration, and contribution changes reach the compiler from
  in-memory zone inputs kept current by watch events, without listing the
  namespace.
- A new zone version writes and sends only the chunks that changed.
- Every version stays a complete, verifiable snapshot, numbered by writer epoch
  and revision, so members still catch up by taking the latest version and
  gaps or reordering stay harmless.
- Each region's writer keeps its own epoch and revision chain.

## Non-goals

- Incremental rule evaluation. The compiler's rules form a conflict graph:
  static record sets claim before registrations, rejection is all or nothing
  per object, contributions depend on claims at every name they write, and SRV
  rejection runs to a fixed point. A change can cascade across names. A full
  compile from memory takes under 5 ms, so phase 1 keeps it.
- Diff-based versions that depend on a base revision.
- Incremental loading inside the serving process.

## Phase 1: in-memory zone inputs

Phase 1 is implemented. Enable both `perZoneReconcile` and `cachedZoneInputs`
to use it. Both options are off by default. See
[per-zone reconciliation](internal-dns-per-zone-reconcile.md) for event routing
and [cached zone inputs](../architecture/internal-dns/cached-zone-inputs.md) for
the implementation contract.

The watch cache supplies record sets and registrations indexed by zone, and
contributions indexed by registration. Associations, naming-policy processing,
resolver contexts, reference pins, and resolver bindings retain their live read
paths. The compiler's input reader is separate from its source API client, but
the implementation does not create regional copies of project objects.

The compiler still evaluates every rule for the complete zone. It caches the
previous plan's chunks by active manifest to avoid rereading unchanged data.
The project pass still manages contexts, associations, naming policies, pins,
bindings, and lease renewal.

### Consistency

Each new writer epoch starts with a successful live compilation. The cache must
catch up with that baseline before the compiler uses cached inputs. The compiler
checks object UIDs, generations, and deletion state; it does not order resource
versions.

Registration and contribution fences retain the versions already published. A
missing contribution requires a live check by name. Missing or outdated
registrations trigger a full live compilation. Any cached result that loses a
previously accepted input also triggers a full live compilation, including when
a dependency across record kinds causes the loss.

Access decisions remain live. Before committing, the compiler rereads the zone,
associations, resolver contexts, reference pins, and resolver bindings. If those
inputs changed, it retries. Status writes use current objects and optimistic
locks.

At a five-minute interval, the compiler compares a cached plan with a live plan,
uses the live result, and counts differences in
`internal_dns_cached_plan_mismatches_total`. After a fallback, the cache must
catch up with the new live baseline.

## Phase 2: content-addressed chunks

This phase is a proposal. The following sections describe its intended format,
delivery rules, and rollout; the current implementation does not provide them.

Today a version's chunks are the compiled plan cut into fixed-size byte
ranges, each tied to one manifest by its UID, epoch, and revision, so any
change rewrites and resends every chunk.

### Chunks

- The writer groups the plan's record sets, owners, and fences by a hash of
  the owner name (fences by the hash of their object UID). A group that grows
  past a size limit splits by the next bits of the same hash, so groups stay
  small as a zone grows and existing groups keep their hashes. A group that
  cannot split further, one owner name with many records, is cut into byte
  ranges of its canonical JSON, each its own hashed chunk.
- Each chunk is canonical JSON, named by its SHA-256, and carries its zone and
  hash, not a manifest identity. This is a new chunk kind; the existing chunk
  kind requires an epoch, revision, and index. It remains available for the old
  form.
- A manifest lists its chunk hashes in order, and still names its zone, epoch,
  revision, and previous revision. The plan hash is the SHA-256 of the canonical
  JSON of the assembled plan, not of concatenated chunk bytes; every reader
  that verifies a plan (manifest verification, the writer's previous-plan load,
  acknowledgment handling, and bootstrap) uses it. Every version is a complete
  snapshot. The fence lists move out of the manifest object into chunks;
  status aggregation reads them from the assembled plan.
- No-op detection compares the ordered chunk hash list plus the serving targets
  and expected regions. Removing or reverting a group adds no chunk but changes
  the list, so it still publishes.

### Delivery

- **Export ledger.** Each zone, region, and shard has a ledger of chunk hashes
  the broker has acknowledged for it. It lives in the publication store and is
  protected from collection.
- **Deduplication.** A writer skips sending a chunk only when its
  hash is in that ledger. A crash or lost commit can leave a staged chunk that
  was never exported; it is not in the ledger, so it is sent.
- **Activation stages what is missing.** Before activating a manifest, the
  writer checks that every chunk it lists has a ledger entry and is present in
  the region's snapshot store. For any that is missing, it drops the ledger
  entry and stages an export for the chunk, and activation depends on that
  export. This also heals a snapshot store that lost data.
- **Members fetch what they lack.** A member that receives a manifest
  referencing a chunk it does not hold reads it by hash from its region's
  snapshot store, at `<region>.<shard>.chunk.<zone>.<hash>`. That covers a long
  outage beyond the stream's retention, a recreated durable and a lease
  takeover. A newly added shard has an empty ledger, so it is sent every chunk.
- **Snapshot store keys.** Hash-keyed chunk keys are created if absent and
  compared by payload hash; manifest heads keep their epoch and revision
  ordering. Bootstrap reads each head and fetches only the chunks it lists.
- **Members.** A member does not store applied chunks. It re-derives an
  applied version's chunk hashes from the applied plan with the same
  deterministic split, and keeps only chunks of pending versions.

### Collection

Chunks are shared between versions, so nothing deletes a chunk with its
manifest. Each store counts references over every manifest it still retains,
including pending ones:

- the publication store, over manifests kept for retention or with pending
  dependencies;
- the regional snapshot store, over current heads and every pending manifest;
- a member, over its pending versions.

A chunk becomes collectable when its count reaches zero and a grace period has
passed since the last manifest that referenced it, so a member bootstrapping
from a head that just moved can still finish. The ledger entry is removed
first, then the chunk; a writer that loses the race finds the entry missing at
activation and re-exports the chunk.

### Fences

Contribution fences are lifetime high-water marks and grow without bound on
both writer and members. Phase 2 bounds them. A retired fence gains a
retirement time, and is dropped only after its contribution is confirmed
absent live (not just terminating) and the retired fence has been in a head
every member acknowledged for longer than the replay window. Members drop it
when it leaves the plan, so member fences gain the zone they belong to.

### Regions

The current implementation has one writer per zone that publishes to several
serving targets. The proposed ledgers and reference counts belong to each zone,
region, and shard. This design supports the current writer model and the
separate proposal for independent regional writers. Each regional writer in
that proposal maintains its own epoch and revision sequence. A new epoch uses
the same lookup rules: the manifest lists every chunk hash, and a member fetches
any missing chunks from its region's store.

## Rollout

- Phase 1 uses the implemented `cachedZoneInputs` option with
  `perZoneReconcile`. The compiler compares cached and live plans at a
  five-minute interval and counts mismatches. Disable `cachedZoneInputs` to
  return to live reads without changing the publication format.
- Phase 2 changes the chunk envelope and the member checkpoint.
  - Members ship first. They read both chunk forms, keep checkpoint format 7,
    and add only optional fields, so a previous release can still load their
    checkpoint and a re-upgraded member tolerates the fields being gone.
  - Members report the chunk forms they accept in their acknowledgments.
    Writers use the new form, behind an option, only for a shard whose members
    all report it. The check is continuous: if a member that reports only the
    old form joins, the writer publishes that shard in the old form again.
  - Rolling back: turn the writer option off on the new release, which
    republishes every zone in the old form; confirm every active manifest and
    snapshot head is old form; only then roll back writer and member binaries.
    An old writer cannot load a new-form previous publication, and an old
    member cannot read new-form chunks.
  - The old form is removed after members have run the new code through one
    restart. Hash-keyed snapshot keys left by a rollback are removed by a
    one-time cleanup, since the old collector does not recognize them.
- Measure each phase with the internal DNS publish test, API request rate,
  and bytes sent per publication.

## Risks

- **Cache lag** can require a live compilation before publication. If the live
  reads fail, the compiler retries without publishing the cached result. The
  periodic comparison counts plan mismatches. Disable `cachedZoneInputs` to
  return to live input reads.
- **A member that cannot reach the snapshot store** cannot apply a version that
  reuses a chunk it lacks. It keeps serving its current version and reports
  the version as unapplied.
- **Reference counting bugs** could collect a chunk a head still needs. The
  writer verifies every head's chunks are present before activating it and
  re-exports any that are missing, and members verify the assembled plan hash.
- **Member checkpoints** are still rewritten whole on every save, so a
  member's checkpoint cost stays proportional to all its zones until the
  checkpoint is split.
