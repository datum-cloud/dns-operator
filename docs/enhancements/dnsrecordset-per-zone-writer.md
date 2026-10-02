# Enhancement: One Writer per Zone for DNS Records

## Summary

Two `DNSRecordSet` objects can claim the same owner name in one zone. The agent writes records one record
set at a time, so it never sees both claims together, and it cannot decide between them. When one of the
two lets the name go, the agent deletes the name, although the other still claims it
([#188](https://github.com/datum-cloud/dns-operator/issues/188)). We propose that the agent writes one
zone at a time instead: it reads every record set of the zone, picks one holder for each name, and writes
the difference in one pass.

## Terms

- A record set **claims** an owner name when it lists that name.
- Of the record sets that claim a name with one record type, the **holder** is the one whose records are
  served: the oldest, and at one creation time, the lower name.
- A record set **releases** a name when it is deleted, or when the name leaves its spec.
- The agent stores an **ownership note** with each record it writes, naming the record set that wrote it.

## Motivation

[`record-ownership.md`](https://github.com/datum-cloud/dns-operator/blob/main/docs/architecture/record-ownership.md) states the rule. When a record set releases
a name, *"the remaining claimants are re-elected … If no claimant remains, the backend record set for
that name is deleted."* Until `v0.7.0` an agent reconciler handled one name at a time, applied this rule,
and told each record set that lost the election `NotOwner`.

[#82](https://github.com/datum-cloud/dns-operator/pull/82) replaced it with one reconcile per record set
([design](https://github.com/datum-cloud/dns-operator/blob/main/docs/enhancements/dnsrecordset-reconcile-consolidation.md)). That fixed a delete that did not survive a restart
([#58](https://github.com/datum-cloud/dns-operator/issues/58)) and a queue that fanned every zone event out
to every name ([#59](https://github.com/datum-cloud/dns-operator/issues/59)). But a reconcile that sees
one record set cannot choose among several. So three behaviours went with the old reconciler: only the
holder's records are written, the others report `NotOwner`, and a released name passes to the next
claimant. [#86](https://github.com/datum-cloud/dns-operator/issues/86) proposes removing the old
reconciler, which nothing runs.

The admission webhook refuses a second claim on a name. It cannot make duplicates impossible:

- it fails open on purpose: in the outage of [#69](https://github.com/datum-cloud/dns-operator/issues/69),
  a webhook set to fail closed refused every record set write while no running release could serve it;
- two creates at the same moment both pass it;
- it does not see claims that existed before it.

[#83](https://github.com/datum-cloud/dns-operator/issues/83) found such pairs in production. So the agent
still has to decide what gets through, as a Gateway API controller does for conflicting routes.

## Goals

- For every name in a zone, the holder's records are served, the other claimants report `NotOwner`, a
  released name passes to the next claimant, and it is deleted when no claimant is left.
- Each step ships as its own release, and the release before it is the rollback. Each step changes at
  most one thing that users see, so a problem points at one step.
- In a zone without two claims on one name, the records served do not change, with one exception:
  records that carry an ownership note although no record set claims them any more. These are leftovers
  of failures such as [#58](https://github.com/datum-cloud/dns-operator/issues/58), and the first pass removes them, within a limit.

## Non-Goals

- Removing records that no record set declares and that carry no ownership note. This is DNSControl's
  default, which its [`NO_PURGE`](https://github.com/StackExchange/dnscontrol/blob/main/documentation/language-reference/domain-modifiers/NO_PURGE.md)
  switch turns off for zones that other systems also write. We would need a per-zone switch of our own
  first.
- Replacing the ownership notes. Until a zone is fully declared, they are the only mark of which records
  the agent may delete.
- Who sets the SOA serial. A zone's SOA and NS records are record sets, which the pass writes like any
  other; the zone controller writes them only when it creates the zone. The serial the SOA carries is a
  separate change ([#101](https://github.com/datum-cloud/dns-operator/issues/101)).

## Background

- **A Kubernetes work queue never processes one item in two workers at once**
  ([client-go](https://pkg.go.dev/k8s.io/client-go/util/workqueue): *"a single item will not be processed
  multiple times concurrently"*). With the zone as the item, each zone has one writer, and no lock is
  needed.
- **Gateway API breaks conflicts by the oldest creation timestamp, then by namespace and name**
  ([`gateway_types.go`](https://github.com/kubernetes-sigs/gateway-api/blob/main/apis/v1/gateway_types.go)).
  Our election uses the same order.
- **DNSControl and octoDNS both build a zone from its declared state and apply the difference.** This
  proposal does the same, limited to the records the agent owns.

## Options

1. **Hand a released name over inside today's per-record-set reconcile.** We built and reviewed this. It
   needs a lock per zone, a separate release path, and the claimants passed down into the PowerDNS client,
   and it still reports no `NotOwner`. It adds code to work around the unit of work.
2. **Rely on the webhook alone.** It cannot close the gaps listed above.
3. **One reconcile per zone and record type.** The item is smaller, but some rules cross types, such as a
   CNAME that may not share a name with other records, and those need the whole zone.
4. **One reconcile per zone.** Chosen. The rule is about several record sets, so the unit of work holds
   them all. A prototype built on 2026-09-29 passed every unit, integration and end-to-end suite, and
   added about as many lines of code as it removed (+288 / −295). On that date the old reconciler
   ([#86](https://github.com/datum-cloud/dns-operator/issues/86)) held about 1,350 more lines, which
   step 5 removes.

## Design

**What is queued.** A zone is queued when one of its record sets is created, deleted, changes its spec,
or starts deleting, and when the zone itself becomes programmed. When a record set moves to another zone,
both zones are queued, so the old zone releases its names. Writes to status and metadata are not queued:
a reconcile that queued its own status writes would run in a loop, the churn behind [#59](https://github.com/datum-cloud/dns-operator/issues/59).

**One pass over a zone:**

1. For each record set of the zone that is not being deleted, add the finalizer and the owner reference,
   and set `Accepted`.
2. Pick the holder of each name and record type, with the same function the webhook uses. The function
   reads the tenant object's creation time, which the replicator copies onto the downstream object. A
   rebuilt downstream cluster recreates every object at once, and ordering by those new times would fall
   back to names and could move a name to another claimant.
3. Send the holders' records to PowerDNS in one call.
4. Set each record set's status for each name it lists. A record set that is not the holder gets
   `Programmed=False` with reason `NotOwner`. A status is patched only when it changed, so an unchanged
   zone makes no writes.
5. Remove the finalizer of each record set that is being deleted, after the write without its names has
   succeeded.

Steps 1, 4 and 5 write to many record sets in a large zone. The pass sends those writes in parallel, at
most 16 at once, so it does not wait on the API server once per record set.

**The call to PowerDNS** replaces `EnsureRecordSet` and `DeleteRecordSet`.

- It reads the zone once.
- It writes a name only when PowerDNS does not already hold the holder's current records.
- It deletes a name that the agent owns and that no record set claims. The agent owns a name when the
  name carries an ownership note from a record set of the zone's own project, or when such a record set
  that is being deleted declares it. So a pass never deletes a record another project wrote, even if two
  projects ever hold one domain.
- A pass that would delete more than 30% of a zone holding at least 10 records deletes nothing. It sets a
  condition of its own on the `DNSZone` instead, which only this reconcile writes. These are octoDNS's
  defaults (`MAX_SAFE_DELETE_PCENT` and `MIN_EXISTING_RECORDS` in
  [`plan.py`](https://github.com/octodns/octodns/blob/main/octodns/provider/plan.py)), and we keep them
  until the dry-run counts below show a reason to differ.
- PowerDNS refuses a whole request when it cannot store one record in it. A refused request is split in
  halves until the refused record is alone, so one record set's bad content fails only its own names.
  Halving costs a few requests where retrying name by name would cost one per name, and each accepted
  request raises the zone's SOA serial.
- Any other failure is returned, and the zone is retried with backoff.

## What users and on-call see

- A record set that loses a name reports `Programmed=False` with reason `NotOwner` on that name, as it did
  before `v0.7.0`. So the `DNSRecordRejected` alert, which fires on such a reason, can fire again
  ([runbook](https://github.com/datum-cloud/infra/blob/main/docs/runbooks/dns/record-rejected.md)).
- When a claimant is deleted or drops a name, the name answers with the next claimant's records, instead
  of going silent.
- A pass stopped by the deletion limit shows as a condition on the `DNSZone`, and nothing is deleted
  until someone looks.

## Phased rollout

Each step is a pull request and a release. Before it merges, it passes the unit, envtest and PowerDNS
integration tests and every Chainsaw suite on three kind clusters. It then runs on staging, watched for
reconcile errors, records programmed, and the drift and `DNSRecordRejected` alerts, before it reaches
production.

1. **One election function**, used by the webhook. No change in behaviour.
2. **The tenant's creation time on each downstream object**, copied by the replicator. No change in
   behaviour until step 4 reads it.
3. **The call to PowerDNS**, added with its integration tests, not yet used. No change in behaviour.
4. **The per-zone reconcile.** The records served change only where a name is claimed twice, and where
   leftovers are removed. A new end-to-end scenario, `claimed-names`, fails before this step and passes
   after it.
5. **Removal of what step 4 made unused:** the per-record-set calls, the old reconciler
   ([#86](https://github.com/datum-cloud/dns-operator/issues/86)), and the ownership parameters on the
   backend interface ([#88](https://github.com/datum-cloud/dns-operator/issues/88)).

Before step 4 reaches staging or production, a dry run of the pass counts, in that environment, the names
claimed twice, the domains that more than one project holds, and the leftovers it would delete. Names
claimed twice in production are removed by hand first, with their owners.

## Risks

- **Scale.** Every change to a record set queues a full pass over its zone, so the largest zone sets the
  cost. Step 4 is load-tested locally with at least as many record sets as production's largest zone,
  and twice its largest burst of new ones, each programmed within the 60-second target of [#59](https://github.com/datum-cloud/dns-operator/issues/59). It then
  runs on staging's largest zone. On 2026-09-29 the largest production zone, which holds gateway
  addresses, had 2,399 record sets, its largest burst in 30 days was 100 new ones in five minutes, and
  staging's largest zone had 1,556. A local test of the prototype on 2026-09-29 met the target: with
  2,500 record sets in one zone, the last of 200 new ones was programmed within 2 seconds, and within
  about 21 seconds with 25 ms added to each API write. The same test is why the pass writes in
  parallel: one at a time, 2,500 new record sets took 115 seconds with 10 ms added to each write, and 8
  seconds with 16 at once.
- **Names claimed twice at rollout.** Step 4 brings `NotOwner`, and with it `DNSRecordRejected`, back. A
  pair left in place pages on-call, and its name moves to the older claimant.
- **Leftovers.** The first pass in each zone deletes records that carry an ownership note but that no
  record set claims. The dry-run count shows how many before the step ships, and the deletion limit stops
  a pass that would delete more.
- **Configuration.** The agent decodes its server config strictly, so it refuses to start on a key it
  does not know. Step 5 removes a key only after no deployed config sets it.
