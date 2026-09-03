# Conditions and Reasons

Every DNS resource reports status through standard Kubernetes conditions. This
document says what each reason means, and answers the question a consumer
actually has: **will this clear on its own, or does a person have to do
something?**

A dashboard that cannot tell those apart shows a spinner over a record that will
never program. An alert rule that cannot tell them apart either pages on normal
convergence or stays silent through a real failure.

## Where conditions live

Zones, record sets, and discoveries carry conditions on `status.conditions`.

A `DNSRecordSet` carries a second layer. Each owner name it holds gets its own
entry in `status.recordSets[]`, with its own `Programmed` condition. **The
interesting reasons only exist there.** Whether a name lost an ownership
contest, or the backend refused it, is per name, because the outcome differs
name by name inside one record set.

The record set's own `Programmed` condition aggregates those entries. On current
`main` it flattens any unfinished record to `Pending`, so the cause is visible
only one level down.

> [!NOTE]
> https://github.com/datum-cloud/dns-operator/pull/116 changes the aggregate to
> carry the blocked record's own reason and name it in the message. The rule in
> [Reading an unfamiliar reason](#reading-an-unfamiliar-reason) is written so it
> holds either way.

## Reasons

`Accepted` says the resource is valid and its dependencies are satisfied.
`Programmed` says the backend has realized the desired state. `DNSZoneDiscovery`
uses `Discovered` in place of `Programmed`.

### Self-clearing

These resolve without anyone touching anything, given time.

| Reason | Where | What it means |
|---|---|---|
| `Pending` | `Accepted`, `Programmed` | No cause has been reported. The resource is still converging, or a dependency has not arrived yet. |
| `PendingDomainVerification` | zone `Accepted` | The domain this zone serves has not completed ownership verification. Clears when verification finishes. |

`Pending` deserves care. It is the absence of a cause, not a promise of
progress. A record set waiting on a zone that nobody ever creates reports
`Pending` forever, and so does one that is converging normally. Treat `Pending`
as healthy only within a time bound, and escalate when it outlasts one.

`PendingDomainVerification` is self-clearing in the DNS service, but the work
that clears it happens elsewhere. It is worth surfacing to the user who owns the
domain, because they are the one who can finish it.

### Needs action

These do not clear until someone changes something. The operator keeps
retrying, and keeps failing, for as long as the cause stands.

| Reason | Where | What it means | Who resolves it |
|---|---|---|---|
| `NotOwner` | per-record `Programmed` | Another record set claimed this owner name first and holds it. | Whoever owns one of the two record sets removes their claim. |
| `Conflict` | per-record `Programmed` | The backend refuses this record because it cannot coexist with data already at the name, such as a `CNAME` beside other types. | Remove the conflicting data, or move the record to another name. |
| `DNSZoneInUse` | zone `Accepted` and `Programmed` | Another zone already claims this domain. | Release the other zone, or use a different domain. |

`NotOwner` is the one most often mistaken for a transient state. It is not.
See [Record Ownership](./record-ownership.md) for who wins a contested name and
when ownership moves.

`Conflict` is polled rather than watched. The conflicting data can be written
straight into the backend, which produces no Kubernetes event, so the operator
re-checks on a long interval instead of hot-looping. A conflict can therefore
clear without any change to a Kubernetes object, and it can take up to that
interval to notice.

### Either

| Reason | Where | What it means |
|---|---|---|
| `PDNSError` | per-record `Programmed` | The PowerDNS backend rejected the change. |

`PDNSError` covers both a record the backend will never accept, such as invalid
record data or a name outside the zone, and a backend that was briefly
unreachable. The first needs a person and the second does not. Only the message
separates them, so pass the message through to whoever is reading rather than
collapsing it to the reason.

### Success

`Accepted`, `Programmed`, and `Discovered` appear as reasons on the conditions
of the same name when the status is `True`.

## Reading an unfamiliar reason

New reasons get added. A consumer written today will meet one it has no mapping
for, and has to decide what to do with it.

**Treat any reason other than `Pending`, an empty reason, or a success reason as
needing attention.**

The rationale is asymmetric cost. A reason added after your code was written is
far more likely to name a new failure than a new kind of success, because
success needs no explanation and failure does. Guessing "still working" hides a
real problem indefinitely; guessing "needs attention" costs one look.

Two consequences worth building in:

- Show the server's own reason and message verbatim when you have no mapping.
  Inventing wording for an unknown reason is how a tool starts lying.
- When folding several per-name statuses into one, let the unhealthy one win.
  A record set is not live while one of its names is failing.

## Related

- [API Reference](./api-reference.md#conditions) for the condition schema.
- [Record Ownership](./record-ownership.md) for the ownership contest behind
  `NotOwner`.
- [Replication Model](./replication.md#status-synthesis) for how status reaches
  the tenant control plane.
