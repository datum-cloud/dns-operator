# Internal DNS incremental zones

A change to one record should cost work in proportion to that record, not to
the size of its zone.

## Why this exists

Every change to a private zone does whole-zone work three times:

- The control plane lists every input in the project namespace and compiles
  the whole zone.
- It writes the whole compiled zone as new chunks and sends every chunk to
  every serving member in each region the zone is published to.
- Each member verifies the whole payload, checkpoints it, renders the whole
  zone file and reloads it.

Compiling is cheap today, under 5 ms for the zones we run. Listing, writing,
sending and loading grow with the zone and the project, and the listing is
what made per-zone reconcile double the control plane's API requests.

## Goals

- Record, registration and contribution changes reach the compiler from
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

This builds on per-zone reconcile (zone keys queued by record events), which
is behind its own option and not yet on main.

Each project keeps the zone inputs it already watches (record sets,
registrations, contributions, associations, naming policies) in its watch
cache, indexed by zone. A zone key compiles from that index instead of listing
the namespace. The plan it produces is exactly what a full compile produces
today. The previous plan is cached in memory by manifest name, so a compile
does not re-read every chunk of the active manifest.

### Consistency

Mixing cached and live reads is what made a plain cache switch unsafe. Each
informer only moves forward, but kinds can lag each other, and a new writer's
cache can lag the writer it replaces. The rules:

- **Fresh start per epoch.** When a writer starts a new epoch for a zone
  (startup or lease takeover), it compiles from a live read of the zone's
  inputs. It switches the zone to cached inputs once, object by object, every
  live input is in the cache with the same UID and an equal or newer
  generation, and every cached input that the live read did not return has
  left the cache. Resource versions are not compared; they are opaque on this
  API. Under constant churn a zone may stay on live reads, which is safe.
- **New rejections are confirmed live.** Cache lag between kinds can make a
  compile reject an input the current publication accepted: a contribution's
  registration missing or at another generation (`StaleRegistrationGeneration`),
  a registration's zone pin not matching (`InvalidReference`), or an SRV
  source losing its target address (`MissingSRVTargetAddress`) because the
  address moved between kinds. Any compile that newly rejects a previously
  accepted input re-reads that input and everything its rejection depended on
  live, substitutes the live objects for the cached ones, and compiles again.
  The result publishes; nothing is held back waiting for the cache.
- **Fences.** Registration and contribution fences in the current publication
  are compared with cached generations before compiling; a cache behind its
  fences waits. A fenced object missing from the cache is confirmed gone with
  one live read by name before the compiler marks its fence retired. This
  replaces today's live list of every contribution on each compile.
- **Access decisions stay live.** The zone, its associations, their resolver
  contexts, reference pins and resolver bindings are re-read live right before
  persisting and compared by UID, generation and the Accepted condition. Any
  difference requeues.
- **Status writes** re-read the object and use an optimistic lock.

The project pass is unchanged by this phase: it still lists its project's
kinds and reconciles contexts, associations, naming policies, pins and
bindings, and queues a zone only when its project-wide inputs change.

## Phase 2: content-addressed chunks

Today a version's chunks are the compiled plan cut into fixed-size byte
ranges, each tied to one manifest by its UID, epoch and revision, so any
change rewrites and resends every chunk.

### Chunks

- The plan's record sets, owners and fences are split into groups by a hash of
  the owner name (fences by the hash of their object UID). A group that grows
  past a size limit splits by the next bits of the same hash, so groups stay
  small as a zone grows and existing groups keep their hashes. A group that
  cannot split further, one owner name with many records, is cut into byte
  ranges of its canonical JSON, each its own hashed chunk.
- Each chunk is canonical JSON, named by its SHA-256, and carries its zone and
  hash, not a manifest identity. This is a new chunk kind; the existing chunk
  kind requires an epoch, revision and index and stays for the old form.
- A manifest lists its chunk hashes in order, and still names its zone, epoch,
  revision and previous revision. The plan hash is the SHA-256 of the canonical
  JSON of the assembled plan, not of concatenated chunk bytes; every reader
  that verifies a plan (manifest verification, the writer's previous-plan load,
  acknowledgement handling, bootstrap) uses it. Every version is a complete
  snapshot. The fence lists move out of the manifest object into chunks;
  status aggregation reads them from the assembled plan.
- No-op detection compares the ordered chunk hash list plus the serving targets
  and expected regions. Removing or reverting a group adds no chunk but changes
  the list, so it still publishes.

### Delivery

- **Export ledger.** Each zone, region and shard has a ledger of chunk hashes
  the broker has acknowledged for it. It lives in the publication store and is
  protected from collection.
- **Dedupe on the ledger only.** A writer skips sending a chunk only when its
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

Main has one writer per zone, publishing to several serving targets. Ledgers
and reference counts are per zone, region and shard, so the design works with
one writer today and with a writer per region once regional control planes
land; each regional writer then keeps its own (epoch, revision) chain. A new
epoch needs nothing special: the manifest lists every chunk hash, and a member
fetches any it lacks from its region's store.

## Rollout

- Phase 1 behind an option on top of per-zone reconcile. A shadow compile from
  live reads runs on resync while the option is new and counts plan-hash
  mismatches.
- Phase 2 changes the chunk envelope and the member checkpoint.
  - Members ship first. They read both chunk forms, keep checkpoint format 7,
    and add only optional fields, so a previous release can still load their
    checkpoint and a re-upgraded member tolerates the fields being gone.
  - Members report the chunk forms they accept in their acknowledgements.
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
    one-off cleanup, since the old collector does not recognise them.
- Each phase is measured with the internal DNS publish test, API request rate
  and bytes sent per publication.

## Risks

- **Cache skew** delays publication by at most one confirming re-read rather
  than withdrawing records; the shadow compile counts any mismatch, and the
  option can be turned off.
- **A member that cannot reach the snapshot store** cannot apply a version that
  reuses a chunk it lacks. It keeps serving its current version and reports
  the version as unapplied.
- **Reference counting bugs** could collect a chunk a head still needs. The
  writer verifies every head's chunks are present before activating it and
  re-exports any that are missing, and members verify the assembled plan hash.
- **Member checkpoints** are still rewritten whole on every save, so a
  member's checkpoint cost stays proportional to all its zones until the
  checkpoint is split.
