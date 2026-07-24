# A DNS record can't be created because of a "conflicting record" error

> [!NOTE]
> **Symptom:** A customer can't create or edit a DNS record. The record's
> status shows it failed to program, with a message like:
>
> ```
> PDNSError: A conflicting record already exists for this name. Remove the
> existing record and try again.
> ```
>
> But nothing in the customer's project references that name anymore.

Every DNS record exists in two places: an **upstream** copy in the
customer's own project control plane, and a **downstream** copy on the
shared DNS infrastructure cluster that gets programmed into the DNS
provider. This doc uses upstream and downstream in that sense throughout.
For details on how this operator is designed, see:
- [Topology](../architecture/topology.md) — the replicator and
  downstream-agent roles, the control planes involved, and discovery modes.
- [Replication](../architecture/replication.md) — the shadow-object model,
  namespace mapping, and status synthesis this doc relies on below.

## What this means

Deleting the upstream record should also clean up the downstream copy. When
that cleanup fails, the downstream copy keeps acting as the owner of that
name, and no upstream object remains for the customer, or for support, to
point to. PowerDNS then rejects any new record request for that name as a
duplicate, indefinitely, because nothing in the system is responsible for
removing the old downstream copy.

## Why this happens

The replicator only cleans up a downstream copy by catching the upstream
record's deletion *while it happens*. If the replicator misses that moment
— for example, during a service restart, or a timing gap right around the
deletion — the downstream copy never learns its upstream is gone. Nothing
later re-checks for that condition. The downstream copy isn't stuck or
crashed; it looks and behaves like a healthy record, which makes it easy to
miss.

This failure mode caused a
[production incident](https://github.com/datum-cloud/engineering/issues/346)
where a customer's `www.ab.dk` couldn't be repointed to a new destination
for about 24 hours, because a downstream record from a previously-deleted
AI Edge was never cleaned up.

## How to investigate

> [!NOTE]
> These steps assume `kubectl` contexts named `upstream` (the customer's
> project control plane) and `downstream` (the shared DNS infrastructure
> cluster). Substitute whatever your environment calls them.

1. **Confirm the upstream record doesn't exist.** List `DNSRecordSet`s in
   the customer's project namespace for the name in question:

   ```bash
   kubectl --context upstream -n <project-namespace> get dnsrecordset \
     -o custom-columns=NAME:.metadata.name,TYPE:.spec.recordType
   ```

   If nothing references the name upstream, but the zone still reports a
   conflict for it, continue to the next step.

2. **Find the downstream namespace for this project.** The replicator maps
   each upstream namespace to a downstream namespace named `ns-<uid>`,
   where `<uid>` is the upstream namespace's `metadata.uid` (see
   [Replication](../architecture/replication.md) for the mapping
   strategy). Look up the value directly instead of guessing it:

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

   Look for a record with the same name the customer is stuck on, showing
   `status.conditions[Programmed]=True`, whose upstream reference no
   longer resolves to anything.

4. **Confirm the record is orphaned.** Every downstream copy carries the
   upstream namespace it came from in an annotation. Check that
   annotation, then verify the namespace (or the record in it) is gone,
   not temporarily unreachable:

   ```bash
   kubectl --context downstream -n "ns-$uid" get dnsrecordset <record-name> \
     -o jsonpath='{.metadata.annotations.meta\.datumapis\.com/upstream-namespace}'

   # then, using that value:
   kubectl --context upstream -n <upstream-namespace-from-above> get dnsrecordset <record-name>
   # expect: Error from server (NotFound)
   ```

   > [!IMPORTANT]
   > If that upstream `get` returns `NotFound` while the downstream copy
   > above is healthy and has no `deletionTimestamp`, you've confirmed an
   > orphan, not a timing artifact. A deleted object leaves no
   > `DeletionTimestamp` behind for anything to react to.

5. **Optional: confirm what the DNS provider has on record** for that
   name, and check it matches what you found downstream, before making
   changes. See [the PowerDNS backend doc](../architecture/backends/powerdns.md)
   for how to query records directly against the backend.

## How to resolve

1. Delete the orphaned downstream record. Deleting it triggers the normal
   cleanup path, removing the record from the DNS provider, instead of
   leaving the provider out of sync:

   ```bash
   kubectl --context downstream -n "ns-$uid" delete dnsrecordset <record-name>
   ```

   > [!WARNING]
   > Before deleting, confirm nothing else references the object (check
   > `metadata.ownerReferences` for an anchor `ConfigMap` — see
   > [Replication](../architecture/replication.md)).

2. Confirm the customer's originally-stuck record programs successfully
   shortly afterward:

   ```bash
   kubectl --context upstream -n <project-namespace> get dnsrecordset <record-name> \
     -o jsonpath='{.status.conditions}'
   ```

## Longer-term fix

This failure mode is a gap in the replicator, not a one-off fluke. The
replicator only knows how to react to *watching* an upstream deletion
happen. It has no way to *discover*, after the fact, that an upstream
record is permanently gone. Until that gap is fixed, expect this failure to
recur under similar conditions, such as a deletion racing a restart or
reconnect. Treat any "conflicting record" report with no matching upstream
object as a candidate for this same root cause.
