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

Each project keeps the zone inputs it already watches (record sets,
registrations, contributions, associations) in its watch cache, indexed by
zone. A zone key compiles from that index instead of listing the namespace.
The plan it produces is exactly what a full compile produces today.

### Consistency

Mixing cached and live reads is what made a plain cache switch unsafe. Each
informer only moves forward, but kinds can lag each other, and a new writer's
cache can lag the writer it replaces. The rules:

- **Fresh start per epoch.** When a writer starts a new epoch (startup or lease
  takeover), its first compile of the zone reads every input live, and the
  zone switches to cached inputs only once each cached kind has reached that
  live read's resource versions.
- **No withdrawal from skew.** A compile that would reject or drop a record
  that the current publication accepted, because of a generation mismatch
  between kinds (a contribution naming a newer or older registration
  generation than the cached one) or a reference pin that does not match, is
  treated as the cache catching up: the zone requeues and keeps its current
  publication. Rejections that do not depend on cross-kind generations, such as
  name conflicts, publish as today.
- **Fences.** Registration and contribution fences in the current publication
  are compared with cached generations before compiling; a cache behind its
  fences waits. A fenced object missing from the cache is confirmed gone with
  one live read before the zone compiles without it.
- **Access decisions stay live.** The zone, its associations, their resolver
  contexts, reference pins and resolver bindings are re-read live right before
  persisting and compared by UID, generation and the Accepted condition. Any
  difference requeues.
- **Status writes** re-read the object and use an optimistic lock.

## Phase 2: content-addressed chunks

Today a version's chunks are the compiled plan cut into fixed-size byte
ranges, each tied to one manifest by its UID, epoch and revision, so any
change rewrites and resends every chunk.

### Chunks

- The plan is split into groups by a hash of the owner name. Owners and fences
  are grouped the same way, so no chunk grows with the whole zone.
- Each chunk is canonical JSON named by its SHA-256 and carries only its zone
  and hash, not a manifest identity.
- A manifest lists its chunk hashes in order and the hash of the assembled
  plan, and still names its zone, epoch, revision and previous revision. Every
  version is a complete snapshot.
- No-op detection compares the ordered chunk hash list plus the serving targets
  and expected regions. Removing or reverting a group adds no chunk but changes
  the list, so it still publishes.

### Delivery

- **Dedupe on acknowledged export.** A writer skips sending a chunk only when
  that chunk was acknowledged by the broker for the same zone, region and shard.
  Presence in the store is not enough: a crash or a lost commit can leave a
  staged chunk that was never exported.
- **Activation waits for every referenced chunk.** A manifest's activation
  depends on the export of every chunk it lists, reused ones included, and no
  chunk export referenced by a pending manifest is ever marked superseded.
- **Members fetch what they lack.** A member that receives a manifest
  referencing a chunk it does not hold reads it by hash from the region's
  snapshot store. This is the path after a long outage beyond the stream's
  retention, a recreated durable, a newly added shard, a lease takeover, or the
  switch to regional writers. The snapshot store keeps every chunk that any
  retained manifest head references, per zone and shard.

### Collection

Chunks are shared between versions, so nothing deletes a chunk with its
manifest. Each store (the publication store, the regional snapshot store, and
a member's checkpoint) keeps a reference count per zone and shard over the
manifests it retains: the current head and the head it replaced. A chunk is
collected when no retained manifest references it. Members count over their
applied and pending manifests.

### Fences

Contribution fences are lifetime high-water marks and grow without bound.
Phase 2 bounds them: a fence for a contribution that has been retired for
longer than the replay window is dropped, together with its contribution's
records, which were already withdrawn.

### Regions

Each region's writer applies the same input events to its own zone inputs and
keeps its own (epoch, revision) chain. Dedupe and reference counts are per zone,
region and shard. A new epoch needs nothing special: the manifest lists every
chunk hash, and a member fetches any it lacks from its region's store.

## Rollout

- Phase 1 behind an option on top of per-zone reconcile. A shadow compile from
  live reads runs on resync while the option is new and counts plan-hash
  mismatches.
- Phase 2 changes the chunk envelope and the member checkpoint. Members ship
  first: they read both chunk forms and write a checkpoint format that the
  previous release can still load, so a member rollback stays possible. Writers
  switch behind an option once every member runs the new code. The old form is
  removed after members have run it through one restart.
- Each phase is measured with the internal DNS publish test, API request rate
  and bytes sent per publication.

## Risks

- **Cache skew** delays publication rather than withdrawing records, by the
  rules above; the shadow compile counts any mismatch, and the option can be
  turned off.
- **A member that cannot reach the snapshot store** cannot apply a version that
  reuses a chunk it lacks. It keeps serving its current version and reports
  the version as unapplied.
- **Reference counting bugs** could collect a chunk a head still needs. The
  writer verifies every head's chunks are present before activating it, and
  members verify the assembled plan hash.
