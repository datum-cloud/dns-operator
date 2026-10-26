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

- Record, registration and contribution changes are applied to in-memory zone
  state from watch events, without listing the namespace.
- Only the names a change touches are re-checked against the zone's rules.
- A new zone version writes and sends only the chunks that changed.
- Every version stays a complete, verifiable snapshot, numbered by writer epoch
  and revision, so members still catch up by taking the latest version and
  gaps or reordering stay harmless.
- Each region's writer keeps its own epoch and revision chain.

## Non-goals

- Diff-based versions that depend on a base revision. They need ordered
  delivery and gap repair on every member.
- Incremental loading inside the serving process. Members still render and
  reload a zone; that can follow once zones with thousands of records exist.

## Phase 1: in-memory zone state

Each project keeps one state object per zone, built from the project's watch
cache: the zone's record sets, registrations, contributions and associations,
indexed by zone and by owner name.

### Consistency

Mixing cached and live reads is what made a plain cache switch unsafe, so the
zone state reads every input from the cache and checks it against what the
zone has already published:

- **Fences for every input.** Publications already carry fences for
  registrations and contributions. Record sets get the same: UID and
  generation. Before compiling, the zone compares the cached generation of
  every fenced object with its fence and waits while any is behind.
- **Missing fenced objects.** A fenced object missing from the cache is
  confirmed gone with one live read by name before the zone compiles without
  it. Registration and record set fences carry the object name for this.
- **References across kinds.** A contribution that names a registration
  generation newer than the cached registration waits for the cache.
- **Access decisions stay live.** The zone, its associations and their
  resolver contexts are re-read live right before persisting, compared by UID,
  generation and the Accepted condition. Any difference requeues.
- **Status writes.** Status patches re-read the object and use an optimistic
  lock.

### Incremental rule checks

The compiler's rules span records: the oldest claimant owns a name, CNAME
excludes other types at its owner, SRV targets must have addresses, tenant
records stay out of reserved scopes, and contributions are fenced. The zone
state keeps an index per rule:

- owner name to claimants, ordered by creation time and UID;
- owner name to record types, for CNAME exclusivity;
- SRV target name to the SRV sources that point at it.

A change recomputes only the owner names it touches, plus SRV sources whose
target lost or gained an address. The output is the same plan a full compile
produces. A full compile runs at startup, when the rule set changes, and as a
periodic shadow check that compares plan hashes and counts mismatches.

Memory is roughly the size of the zone's inputs: a few hundred bytes per
record set, about 30 MB for 1,000 zones of 100 record sets.

## Phase 2: content-addressed chunks

Today a version's chunks are the compiled plan cut into fixed-size byte
ranges, so any change rewrites every chunk after it.

- **Stable chunks.** The plan is split into groups of record sets by a hash of
  the owner name, plus one header chunk with the zone's owners and fences.
  Each chunk is canonical JSON and is named by its SHA-256.
- **Manifests list hashes.** A manifest still names its zone, epoch, revision
  and previous revision, and lists its chunk hashes in order with the hash of
  the assembled plan. Every version remains a complete snapshot.
- **Write and send only new chunks.** The writer stores and sends a chunk only
  when its hash is not already in that region's store. Unchanged groups are
  reused by hash.
- **Members keep chunks by hash.** The checkpoint and bootstrap key chunks by
  hash instead of by manifest and index, and keep the chunks the applied and
  pending manifests reference.
- **No-op detection.** The state hash becomes the ordered chunk hashes plus the
  serving targets and expected regions. No new chunks and no target change
  means no new version.

### Regions

Each region's writer applies the same input events to its own zone state and
keeps its own (epoch, revision) chain. A chunk is reused only if that region's
store already holds it. A new epoch, from a lease takeover or a region's first
publication, needs nothing special: the manifest lists every chunk hash, and a
member fetches any it lacks.

## Rollout

- Phase 1 behind an option on top of per-zone reconcile. The shadow full
  compile runs for every zone while the option is new.
- Phase 2 needs serving members that understand hash-keyed chunks before any
  writer produces them. Members ship first and accept both forms; writers
  switch behind an option; the old form is removed after every member has run
  the new code through one restart.
- Each phase is measured with the internal DNS publish test, API request rate
  and bytes sent per publication.

## Risks

- **A missed index update** produces a plan that differs from a full compile.
  The shadow check counts it and the option can be turned off.
- **Cache lag** delays a publication but cannot roll one back while every
  input is fenced.
- **Chunk store growth.** Chunks no manifest references are removed once no
  applied or pending manifest in that region names them.
