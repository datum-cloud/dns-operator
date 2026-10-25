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

### 2. Ignore status-only self-events

Only queue a pass for a create, a delete, or an update that changes generation,
deletion time, finalizers or labels. A pass that writes a status an earlier phase
of the same pass reads, such as a managed association's acceptance, returns a
short requeue so bootstrap does not wait for the periodic pass.

### 3. Skip phases whose inputs did not change

Keep the single `project` key. The event handler records which kinds changed
since the last successful pass, and which zones the changed record sets name;
it uses both the old and new object on update, so a record set that moves from
zone A to zone B marks both. When only record sets changed, a pass:

- reads ownership of every private zone and renews leases as today;
- lists record sets, registrations and contributions fresh, since a record set
  can take a name from a registration and change that registration's status;
- skips the context, association, naming policy, pin, reference collection and
  binding phases, using the values the last pass established;
- compiles only the zones it marked, and writes status and publication status
  for them as today.

Any other changed kind, a missing record of the last pass, or a zone whose UID,
generation or finalizer differs from the last pass runs the full pass.

A per-zone input fingerprint (zone UID and generation, and the resource versions
of its record sets, registrations, contributions, association pins and serving
targets) also lets a full pass skip reading the previous plan for zones that
cannot have changed.

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
  compare the plan hash each would have produced for the same zone, logging any
  divergence.

## Risks

- **Skipped phases going stale.** A skipped phase reuses values from the last
  pass. Anything that could change them must mark its kind dirty, and every
  input that is not watched (ownership, manifests, chunks, resolver bindings,
  reference state) is still read live, as today.
- **Status from stale copies.** Status writes keep reading the object first and
  patching with an optimistic lock where another writer shares the status.
- **Two replicas.** Both replicas receive every event; only the zone lease
  holder compiles, as today.
