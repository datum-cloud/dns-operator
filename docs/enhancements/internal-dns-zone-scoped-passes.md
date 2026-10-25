# Internal DNS zone-scoped passes

A change to one private record set should publish only that record set's zone,
without first redoing every resolver context, binding and zone in the project.

## Why this exists

The internal DNS control plane reconciles each project with a single queue key.
Every event in the project, from a resolver context to one TXT record, starts the
same full pass:

1. resolver contexts and their managed zones
2. lists of zones, associations, registrations, contributions, record sets and
   naming policies
3. associations, naming policies and reference pins
4. resolver bindings for every context
5. ownership of every private zone, then compiling and persisting each zone whose
   content changed
6. status of every input

A record change waits for any pass already running, then for a whole pass of its
own. In a test deployment a pass took 0.5 s at p50 and 3.5 s at p90, and a large
project took 15 to 60 s. That wait is now the biggest part of the time from a
record write to the record being answered: about 2.5 to 4.5 s at p50 for the
test project, against about 0.2 s for the serving fleet to load a publication.

## Proposal

Keep the full project pass, and add a zone pass for changes that can only affect
one zone.

- **Queue keys.** A project controller queues either `project`, as today, or
  `zone/<name>`. It still runs one key at a time (one worker per project), so a
  zone pass never overlaps the project pass or another zone pass.
- **Which events become zone keys.** A `DNSRecordSet` event queues the zone its
  `spec.dnsZoneRef` names. Everything else keeps queuing `project`: zones,
  associations, resolver contexts, access bindings, naming policies,
  registrations and contributions. Registrations and contributions affect pins,
  names and fences across zones, and moving them is a follow-up once record sets
  are proven.
- **What a zone pass does.** Read the zone, the record sets in the namespace that
  name it, and the zone's ownership record. If this replica holds the zone's
  lease, compile and persist the zone with the inputs the last project pass
  established (associations and their pinned references, registrations,
  contributions and serving targets), then patch the status of that zone's record
  sets. If it does not hold the lease, do nothing; the holder sees the same
  event.
- **When a zone pass falls back.** The controller remembers, per zone, the
  generation of the zone and the set of association, registration and
  contribution versions the last project pass compiled against. If the zone's
  generation differs, the zone is deleting, there is no remembered project pass,
  or the lease is due for renewal, the zone pass queues `project` instead and
  returns. The project pass stays the only place that renews leases and touches
  shared state.
- **Periodic safety net.** The project pass still runs on every non-record event
  and on its existing requeue, so anything a zone pass skipped is reconciled on
  that cadence.

## What it changes for users

A record set change publishes after a zone pass: one list, one compile and one
persist, a few hundred milliseconds in a small project. Large projects benefit
most, because their record changes stop paying for their many contexts and
bindings. Changes to zones, networks, access and registrations behave as today.

## Rollout

- Behind a control-plane option, `zoneScopedPasses`, off by default.
- Enable it in a test deployment and compare publish latency and pass time with
  the internal DNS publish test, before and after.
- Watch for zones that compile differently from a zone pass than from a project
  pass. A debug mode that runs both and compares the plan hashes would catch a
  divergence before it is served.

## Risks and open questions

- **Remembered inputs going stale.** A zone pass compiles against inputs from the
  last project pass. Anything that changes those inputs must queue `project`,
  and the fallback check must catch anything that slips through. The plan-hash
  comparison above is the backstop.
- **Two replicas.** Both replicas receive the event. Only the lease holder
  compiles, as today.
- **Memory.** The remembered inputs are per zone and hold names and versions,
  not objects.
- **Contributions and registrations.** Product publishers write these, and they
  will dominate change volume for service names. They are out of scope here and
  should get their own proposal once record sets have run zone-scoped for a
  while.
