# Internal DNS per-zone reconciliation

A change to a private record should publish its zone directly, without the control
plane first redoing every resolver context, binding and zone in the project.

## Why this exists

The internal DNS control plane reconciles each project with a single queue key.
Every event in the project, from a resolver context to one TXT record, starts the
same full pass over the project's contexts, managed zones, associations, naming
policies, reference pins, resolver bindings and zones. Every read goes to the API
server; the manager's caches only deliver watch events.

In a test deployment a pass took 0.4 s at p50 and 1.9 s at p90, and the busiest
project took 10 to 20 s. A changed record waited for that pass: 3 to 4.5 s at p50
from the write to the record being answered. Compiling a zone takes under 5 ms
and the serving fleet loads a publication in about 0.2 s. Nearly all of the time
is the pass.

## Proposal

Split the project pass into two controllers with clear ownership, and let record
changes reach only the zone controller.

### Ownership

| Controller (key) | Owns | Runs on |
| --- | --- | --- |
| Project controller (project) | Resolver context status, managed zones and associations, association status and pins, naming policy status, access and resolver bindings, destination claims and their release, reference-state cleanup, zone lease renewal | Changes to contexts, access bindings, associations, zones, naming policies; the periodic resync; lease renewal |
| Zone controller (zone) | Registration pins, compiling and publishing the zone, record set status, registration and contribution acceptance, contribution publication status | Changes to record sets, registrations, contributions; the project controller queuing the zone |
| ACK sink (unchanged) | Zone and registration Published and Available | Member acknowledgements |

The project controller is today's pass without the zone compile. It keeps
everything that is decided across a whole project: context identity and
duplicate consumer IDs, apex conflicts between associations, and releasing
destination claims, which needs every access binding in the project at once. It
queues every zone whose inputs it changed: associations and their pins, a zone's
domain or generation, and resolver bindings and their serving targets.

The zone controller is today's `compileZone` with its own preparation. Before
compiling, it pins its zone's registrations. If any association pin is behind the
zone's generation, it queues the project controller and returns, so a zone is
never compiled against stale pins.

### Events

Both controllers use explicit predicates:

| Watched kind | Predicate | Queues |
| --- | --- | --- |
| DNSRecordSet | create, delete, generation | the old and new zone it names |
| DNSRegistration (new watch) | create, delete, generation | the old and new zone it names |
| DNSRecordContribution | create, delete, generation | its registration's zone |
| DNSZoneAssociation | create, delete, generation, status `Accepted` | project |
| DNSZone | create, delete, generation, finalizers | project, and the zone |
| DNSResolverContext, DNSResolverAccessBinding | create, delete, generation | project |
| DNSNamingPolicy | create, delete, generation | project |

Status written by this controller never queues work. Status written by the ACK
sink reaches the zone controller in process: the sink queues a zone when its
publication verdict changes, which replaces today's published-recheck timer.
Resolver bindings live in the platform namespace and are not watched. The project
controller queues the affected zones after it writes bindings, and the ACK sink
does the same when a binding's Serving condition changes.

Zone keys are queued with a 50 to 100 ms delay, so a burst of writes to one zone
becomes one publication.

### Reads from the watch cache

The zone controller reads record sets, registrations, contributions and
associations from the project's watch cache. Registrations join the cache, and
the cache starts with every kind it serves. Today every read is uncached, so that
a partition cannot publish from stale contexts, access bindings, associations,
registrations or producer observations. These checks keep that guarantee:

- **Fences:** before compiling, the zone controller compares the cached
  generation of each registration and contribution with the fences in the zone's
  previous publication. If the cache is behind what was already published, it
  requeues. A lagging cache can therefore never withdraw a published record.
- **Access-deciding inputs:** right before persisting, it re-reads the zone, its
  associations and their resolver contexts from the API server. It compares
  UIDs, generations and the relevant conditions, not resource versions, because
  the ACK sink changes those on every publication. If any of them differ, it
  requeues.
- **The rest of the guarantee is unchanged:** the project controller keeps its
  fresh reads of access bindings, identity is verified per project, and the lease
  and version fence still make every publish conditional on a fresh,
  optimistically locked ownership read.
- **Status patches:** registration status patches re-read the object and use an
  optimistic lock, as `aggregatePublicationStatus` does today, so a cached copy
  cannot overwrite the ACK sink's conditions.

Platform-side reads (pins, resolver bindings and manifests) move to one shared
platform cache, filtered by label and indexed by project. Chunks are never
cached, because they are large and unlabelled.

### Leases and concurrency

The project controller renews every zone lease the replica holds, as today, on
the lease/2 cadence. That is an ownership read and patch per zone, without
compiling.

The zone controller runs several reconciles at once per project. A process-wide
limit caps how many run across all projects, in place of today's pass slots.

Shared client budgets bound throughput. Renewal costs about one platform request
per zone every lease/6. Before projects with thousands of zones, the platform
client limit or the renewal design (for example one lease per project) needs
revisiting.

### Periodic resync

The project controller resyncs every few minutes. A resync queues every zone key
instead of compiling itself, so only the zone controller ever compiles a zone,
and resync and event-driven work cannot race.

## Expected effect

At p50, from a write to the answer, for updates and deletes:

| Stage | Today | Expected |
| --- | --- | --- |
| Accept the write | 0.05–0.1 s | 0.05–0.1 s |
| Wait for and run the reconcile | 0.4–3 s | 0.15–0.3 s |
| Persist the publication | about 0.25 s | about 0.25 s |
| Send to the broker | 0.1–0.5 s | about 0.1 s |
| Serving loads and answers | about 0.2 s | about 0.2 s |
| **Total** | **3–4.5 s** | **about 0.8–1.0 s** |

Getting under 0.8 s also needs the previous plan cached per zone by manifest
name, record set status patched after persist, and fenced contribution status
re-read only for contributions that changed. Creates also pay the API server's
admission for record sets, 0.4 to 0.9 s today, which is outside this controller.

## Rollout

- Behind a control-plane option, `perZoneReconcile`, set for the whole
  deployment. During a rollout, replicas on different settings stay safe for zone
  publication because of the lease. The project controller's bindings and claims
  work is the same code in both modes.
- Enable it in a test deployment and compare publish latency, API request rate and
  reconcile time with the internal DNS publish test.
- While it is new, the resync also compiles each zone from fresh reads and
  compares the plan hash with the zone controller's last publication. It counts a
  mismatch only when it persists across two resyncs, because a queued event can
  legitimately be in flight. A mismatch is logged and counted in a metric, the
  zone is queued, and the alert tells an operator to turn the option off.

## Implementation status

The first cut, behind `perZoneReconcile`, ships the event routing and leaves the
read path as it is:

- Built: zone keys for record sets, registrations (a new watch) and
  contributions, queued after 75 ms; contribution events forward to their
  registration's zone key, so a zone compiles on one worker at a time; the
  project pass renews leases and hands every held zone to its key; a zone key
  waits while any of its associations is pinned against an older zone
  generation; a process-wide limit on concurrent zone compiles.
- Zone keys read associations, registrations, contributions and record sets
  from the project's watch cache. Before compiling, a zone key compares the
  cached generation of each registration and contribution with the fences in
  the zone's current publication and waits if the cache is behind. The zone
  itself, association pins, leases and the contribution confirmation still read
  live. Zone keys reuse a project identity check for up to 10 s.
- Not yet built: the fresh re-read of associations and contexts right before
  persisting; optimistic locks on input status patches; the ACK sink queuing
  zones; the shadow compile on resync.

## Regions

Every region must keep publishing when another region or the central control
plane is down. Zone keys do not change where publications are written, so this
proposal does not make that true on its own. A per-region writer would need:

- zone inputs readable in the region, from a regional replica of the project
  API or a regional publication store;
- a lease and writer epoch per zone and region, so regions fence independently;
- acknowledgements consumed and aggregated in the region, with project status
  merged from regions rather than gating any region's publication.

## Risks

- **Missed mappings.** An input that can change a zone but is not in the event
  table leaves that zone stale until the next resync. Adding an input to the
  compile means adding a row.
- **Two controllers on one project.** They write disjoint objects and statuses,
  as the ownership table shows, and talk through queue keys.
- **More, smaller reconciles under churn.** A burst across many zones runs many
  small reconciles instead of one pass. The per-zone delay coalesces repeats, and
  the process-wide limit bounds concurrency.
