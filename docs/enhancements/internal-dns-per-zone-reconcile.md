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

Reconcile the zone, not the project, and react to each change.

### Per-zone reconciler

A controller keyed by private zone recomputes one zone from current state and
publishes it, exactly as `compileZone` does today. Events map to the zones they
can affect:

| Event | Zones queued |
| --- | --- |
| DNSRecordSet | the zone it names (on update, the old and new zone) |
| DNSRegistration, DNSRecordContribution | the zone of the registration |
| DNSZoneAssociation | the zone it names |
| DNSZone | itself |
| DNSResolverContext, resolver binding changes | every zone associated with the context |
| DNSNamingPolicy | every zone the policy's associations attach |

Each zone reconcile reads its inputs, holds or renews its lease, compiles,
persists, and writes the status of that zone's inputs. Everything that publishes
a zone still goes through the same lease and version fence.

### Per-context reconciler

Network work moves to its own controller keyed by resolver context: the managed
zone and association, access bindings, resolver bindings and destination claims.
It runs only when the context, its access bindings or its managed objects change,
and queues the context's zones when their serving targets change.

### Reads from the watch cache

Both controllers read their inputs from the manager's watch cache instead of the
API server. Today every read is uncached, so that a partition cannot publish
from stale contexts, access bindings, associations or registrations. Two things
keep that guarantee:

- the lease and version fence still make a zone's write conditional on a fresh,
  optimistically locked read of its ownership record;
- right before persisting, the zone reconciler re-reads from the API server the
  inputs that decide access and authority: the zone, its associations and the
  resolver contexts they reference. If any differs from the cached copy, it
  requeues instead of publishing.

**This is the change reviewers should weigh.** It removes most API calls from the
hot path, and it moves the partition guarantee from "every read is fresh" to "the
reads that decide access are fresh at write time".

### What stays as a safety net

- **Whole-zone versions:** serving never loads half a change, and catching up
  after a restart is loading the latest version.
- **One writer per zone (lease and version fence):** two replicas cannot publish
  conflicting versions.
- **A periodic full resync** of every zone and context, every few minutes:
  catches missed events and changes made while a lease holder was down.

### What goes

- The per-project pass that every event triggers, and its project-wide phases
  for record changes.
- The pass slots and phase plumbing that existed to make that pass affordable.
- Most uncached API reads, and the self-triggered passes from status writes.
  Status writes from this controller no longer queue work; status from the ACK
  sink still does.

## Expected effect

From a write to an answer, at p50, for updates and deletes:

| Stage | Today | Expected |
| --- | --- | --- |
| Accept the write | 0.05–0.1 s | 0.05–0.1 s |
| Wait for and run the reconcile | 0.4–3 s | about 0.05 s |
| Persist the publication | about 0.25 s | about 0.25 s |
| Send to the broker | 0.1–0.5 s | about 0.1 s |
| Serving loads and answers | about 0.2 s | about 0.2 s |
| **Total** | **3–4.5 s** | **about 0.7 s** |

Creates also pay the API server's admission for record sets, which is 0.4 to 0.9 s
today and outside this controller.

## Rollout

- Behind a control-plane option, `perZoneReconcile`, off by default. When on, the
  project pass stops queuing on events and runs only as the periodic resync.
- Enable it in a test deployment and compare publish latency, API request rate
  and pass time with the internal DNS publish test.
- While it is new, the periodic resync compares the plan hash it computes with
  the last published one for each zone. A mismatch is logged, counted in a
  metric, republished from the full computation, and turns the option off for
  that project until restart.

## Risks

- **Missed mappings.** An input that can change a zone but is not in the event
  table leaves that zone stale until the next resync. The table above lists every
  input `compileZone` reads today; adding an input means adding a mapping.
- **Cached reads.** A stale cache cannot publish access a context no longer has,
  because those inputs are re-read before the write. It can delay a change until
  the cache catches up, which the resync bounds.
- **More, smaller reconciles under churn.** A burst of changes across many zones
  now runs many small reconciles instead of one large pass. Each is cheap from the
  cache, and the workqueue coalesces repeats per zone.
