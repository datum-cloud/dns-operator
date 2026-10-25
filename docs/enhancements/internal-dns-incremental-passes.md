# Internal DNS incremental passes

A change to one private record set should publish its zone without the control
plane first redoing every resolver context, binding and zone in the project.

## Why this exists

The internal DNS control plane reconciles each project with a single queue key.
Every event in the project, from a resolver context to one TXT record, starts the
same full pass. The pass first lists zones, associations, registrations,
contributions, record sets and naming policies. Then it reconciles resolver
contexts and their managed zones, associations, naming policies, reference pins
and resolver bindings. After that it holds ownership of every private zone,
compiles and persists each zone whose content changed, and writes status.

Every read goes to the API server; the manager's caches only deliver watch
events. In a test deployment a pass took 0.5 s at p50 and 3.5 s at p90, and a
large project took 15 to 60 s. Waiting for and running passes is now the largest
part of the time from a record write to the record being answered: 2.5 to 4.5 s
at p50 for a small test project, against about 0.2 s for the serving fleet to
load a publication.

Three things make passes slower and more frequent than they need to be.

- **Per-zone repeats.** Some reads give the same answer for every zone but run
  once per zone: each association's reference state, the namespace's
  contributions, the project's resolver bindings (twice), and the zone's
  previous plan. A project with many zones pays these many times per pass.
- **Self-triggered passes.** The controller watches the record sets,
  associations, contexts and access bindings whose status it patches. Each record
  write therefore causes a second, no-op pass, and a write landing during that
  pass waits for it.
- **All-or-nothing passes.** A record set change re-runs the context, association,
  policy, pin, reference and binding phases, even though none of their inputs
  changed.

## Proposal

Three steps, each measured before the next.

### 1. Read per-pass inputs once

Compute association references, confirmed contributions and the project's
resolver bindings once per pass, and share them across zones. This changes no
behaviour: every value is the same authoritative read, made once instead of once
per zone.

### 2. Ignore the controller's own status writes

Skip an update only when it changes nothing but status and the status was written
by this controller, identified by its field manager in the object's managed
fields. Status written by anyone else still queues a pass. The ACK sink, for
example, writes resolver binding member acknowledgements and registration
publication conditions that a pass reads (`bindingAckCurrent`,
`aggregatePublicationStatus`). Creates, deletes and changes to generation,
deletion time, finalizers or labels always queue a pass.

A pass that writes a status that an earlier phase of the same pass reads, such
as a managed association's acceptance, returns a short requeue. Otherwise
bootstrap would wait for the next lease-renewal pass.

### 3. Skip phases whose inputs did not change

Keep the single `project` key. The event handler keeps a dirty set: the kinds
that changed since the last successful pass, and the zones the changed record
sets name. On update it uses both the old and the new object, so a record set
that moves from zone A to zone B marks both zones.

These kinds are dirty kinds. A change to any of them runs the full pass:
DNSZone, DNSZoneAssociation, DNSResolverContext, DNSResolverAccessBinding,
DNSNamingPolicy, DNSRegistration and DNSRecordContribution, plus resolver
binding status from the ACK sink.

When only record sets changed, a pass:

- reads ownership of every private zone and renews leases as today;
- lists record sets, registrations and contributions fresh, because a record set
  can take a name from a registration and change that registration's status;
- skips the context, association, naming policy, pin and reference collection
  phases and the writes of the binding phase, reusing the values from the last
  full pass;
- still reads resolver bindings live for publication targets, since a stale
  binding set would send a zone to the wrong regions;
- compiles only the zones it marked, and writes their status and publication
  status as today.

The full pass runs instead whenever:

- a dirty kind changed;
- this replica has no record of a last full pass, as after a restart or on a new
  lease holder;
- a zone's UID, generation or finalizer differs from the last pass;
- a zone has a publication target change or withdrawal pending;
- a full resync is due. A full pass runs at least every few minutes regardless,
  so changes that never produced an event, such as a missed watch or a change
  made while the lease holder was down, are corrected within that bound.

A pass that fails or loses a lease partway keeps its dirty set, merged into the
next pass.

A per-zone fingerprint can also let a full pass skip reading the previous plan
for zones that cannot have changed. It covers the zone's UID and generation, the
generations of its record sets, registrations and contributions, its association
pins, and its current and previous publication targets. Generations are used
rather than resource versions so that status writes don't count as changes.

## Why not a second queue key per zone

A `zone/<name>` key was considered and rejected. It needs a second place that
renews leases or a fallback for every renewal, its keys queue ahead of the
project key, and under churn across many zones each key re-pays its own setup.
Step 3 gets the same saving with one key and the existing lease handling.

## What it changes for users

A record set change publishes after a pass that reads and compiles only what the
change can affect. Large projects benefit most, because their record changes
stop paying for their many contexts, bindings and zones. Changes to zones,
networks, access, naming and registrations behave as today.

## Rollout

- Steps 1 and 2 change no behaviour and ship on their own.
- Step 3 ships behind a control-plane option, `incrementalPasses`, off by
  default. Enable it in a test deployment and compare publish latency, pass time,
  queue wait and self-triggered pass counts with the internal DNS publish test.
- While the option is new, run a full pass every N incremental passes, and
  compare the plan hash each would have produced for the same zone. A divergence
  is logged, counted in a metric, and turns incremental passes off for that
  project until the next restart. The full pass's plan is the one published.

## Risks

- **Skipped phases going stale.** A skipped phase reuses values from the last
  full pass. Any watched input that could change them marks its kind dirty.
  Ownership, manifests, chunks, resolver bindings and reference state are not
  watched and are still read live, as today. The time-based full resync bounds
  anything that slips through.
- **Status from stale copies.** Status writes keep reading the object first and
  patching with an optimistic lock where another writer shares the status.
- **Two replicas.** Both replicas receive every event; only the zone lease
  holder compiles, as today.
