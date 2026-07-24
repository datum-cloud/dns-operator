# A DNS record can't be created because of a "conflicting record" error

**Symptom:** A customer can't create or edit a DNS record. The record's
status shows it failed to program, with a message like:

```
PDNSError: A conflicting record already exists for this name. Remove the
existing record and try again.
```

...but nothing visible in the customer's project actually references that
name anymore.

Background on how DNS records are represented across control planes (and
how to access each one) is covered in the wiki's
[Multi-Tenancy: Upstream & Downstream Control Planes](https://wiki.datum.net/infrastructure/dns/multi-tenancy)
doc — read that first if the upstream/downstream terminology below is
unfamiliar. For how this operator itself is designed, see this repo's
architecture docs:
- [Topology](../architecture/topology.md) — the replicator and
  downstream-agent roles, the control planes involved, and discovery modes.
- [Replication](../architecture/replication.md) — the shadow-object model,
  namespace mapping, and status synthesis this doc relies on below.

## What this means

Every DNS record has an upstream copy (what the customer sees) and a
downstream copy (what actually gets written to the DNS provider). Deleting
the upstream record is supposed to clean up the downstream copy too. When
that cleanup is skipped, the downstream copy keeps quietly reasserting
itself as the owner of that name — with no upstream object left for the
customer, or for us, to point at from their side. Any new record request
for that same name then gets rejected as a duplicate, indefinitely, because
nothing in the system still considers itself responsible for removing the
old one.

## Why this happens

The cleanup of a downstream copy currently depends on catching the upstream
record's deletion *while it's happening*. If that moment is missed — for
example, because of a service restart or a timing gap right around when the
record was deleted — the downstream copy is never told its upstream is
gone, and nothing later re-checks for that condition on its own. It's not
a stuck or crashed object; it looks and behaves perfectly healthy, which is
part of what makes it easy to miss.

This caused a [production incident](https://github.com/datum-cloud/engineering/issues/346)
where a customer's `www.ab.dk` couldn't be repointed to a new destination
for about 24 hours, because a downstream record from a previously-deleted
AI Edge was never cleaned up.

## How to investigate

These steps assume `kubectl` contexts named `upstream` (the customer's
project control plane) and `downstream` (the shared DNS infrastructure
cluster) — substitute whatever your environment actually calls them.

1. **Confirm the upstream record really doesn't exist.** List
   `DNSRecordSet`s in the customer's project namespace for the name in
   question:

   ```bash
   kubectl --context upstream -n <project-namespace> get dnsrecordset \
     -o custom-columns=NAME:.metadata.name,TYPE:.spec.recordType
   ```

   If nothing there references the stuck name, but the zone still reports
   a conflict for it, that's the signal to keep going.

2. **Find the downstream namespace for this project.** The replicator maps
   each upstream namespace to a downstream namespace named `ns-<uid>`,
   where `<uid>` is the upstream namespace's `metadata.uid` (see
   [Replication](../architecture/replication.md) for the mapping
   strategy). Look it up directly instead of guessing:

   ```bash
   uid=$(kubectl --context upstream get namespace <project-namespace> \
     -o jsonpath='{.metadata.uid}')
   echo "ns-$uid"
   ```

3. **Look for a leftover downstream copy** in that namespace on the shared
   DNS infrastructure cluster:

   ```bash
   kubectl --context downstream -n "ns-$uid" get dnsrecordset <record-name> -o yaml
   ```

   You're looking for a record that claims the same name the customer is
   stuck on, is otherwise healthy (`status.conditions[Programmed]=True`),
   but whose upstream reference no longer resolves to anything.

4. **Confirm it's actually orphaned.** Every downstream shadow carries the
   upstream namespace it came from in an annotation. Check it, then verify
   that namespace (or the record in it) is really gone, not just
   temporarily unreachable:

   ```bash
   kubectl --context downstream -n "ns-$uid" get dnsrecordset <record-name> \
     -o jsonpath='{.metadata.annotations.meta\.datumapis\.com/upstream-namespace}'

   # then, using that value:
   kubectl --context upstream -n <upstream-namespace-from-above> get dnsrecordset <record-name>
   # expect: Error from server (NotFound)
   ```

   If that upstream `get` returns `NotFound` while the downstream copy
   above is healthy and has no `deletionTimestamp`, this is a confirmed
   orphan, not a timing artifact — a genuinely deleted object leaves no
   `DeletionTimestamp` behind for anything to react to.

5. **Optional:** confirm what the DNS provider itself currently has on
   record for that name, to double check it matches what you found
   downstream before touching anything. See
   [the PowerDNS backend doc](../architecture/backends/powerdns.md) for how
   records are queried directly against the backend.

## How to resolve

1. Delete the orphaned downstream record — this should trigger the normal
   cleanup path (removing the record from the DNS provider), not leave it
   out of sync:

   ```bash
   kubectl --context downstream -n "ns-$uid" delete dnsrecordset <record-name>
   ```

   Confirm nothing else references the object first (check
   `metadata.ownerReferences` for an anchor `ConfigMap` — see
   [Replication](../architecture/replication.md) — before deleting).

2. Confirm the customer's originally-stuck record programs successfully
   shortly afterward:

   ```bash
   kubectl --context upstream -n <project-namespace> get dnsrecordset <record-name> \
     -o jsonpath='{.status.conditions}'
   ```

## Longer-term fix

This is a gap, not a one-off fluke: the system only knows how to react to
*watching* an upstream deletion happen, not to *discovering* after the
fact that an upstream record is permanently gone. Until that's fixed,
expect this to recur under similar conditions (deletions racing with a
restart or reconnect), and treat any similar "conflicting record" report
with no matching upstream object as a candidate for this same root cause.
