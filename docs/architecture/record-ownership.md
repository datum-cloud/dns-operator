# Record Ownership

A `DNSRecordSet` holds records of one type for one or more owner names inside a
zone. Nothing stops two record sets in the same zone from listing the same owner
name, and both are valid objects. Only one of them is written to the backend.

This document states which one wins, what the other reports, and how the
operator decides whether two records name the same thing.

## The ownership key

Ownership is decided per **(zone, record type, owner name)**, not per zone and
not per `DNSRecordSet`.

A record set that lists five owner names holds five independent claims. It can
own three of them and lose the other two, and its status says so name by name.
Two record sets that list the same name under different record types do not
compete at all: an `A` record for `www` and a `TXT` record for `www` are
separate keys, and both are programmed.

The zone half of the key is the `DNSZone` a record set references, within the
namespace that zone lives in. Record sets in different namespaces, or pointing
at different zones, never contend.

## Who wins

The backend agent reconciles one key at a time. For each key it gathers every
record set in the zone's namespace that references the zone, carries the
matching record type, and lists that owner name in its spec. From that set it
elects a single holder:

1. Oldest `metadata.creationTimestamp` wins.
2. If two share a timestamp, the lower `metadata.name` wins.

The election is first-come and it is stable. A record set created later never
displaces the holder, however its records change, and re-running the election
on every reconcile always returns the same answer while the same claimants
exist.

Only the holder's records for that name are sent to the backend. Records that
other claimants list under the same name are not merged in, not appended, and
not written.

## When ownership moves

Ownership changes only when the holder stops claiming the name. That happens
when the holder is deleted, or when its spec is edited so the name is no longer
listed.

On the next reconcile of that key, the remaining claimants are re-elected by the
same two rules, and the new holder's records are written over whatever the
previous holder left. There is no handover signal and no grace period, so a
brief window exists where the name still resolves to the old holder's data.

If no claimant remains, the backend record set for that name is deleted, subject
to the aliasing guard described below.

## What a losing record reports

Every claimant gets a per-name entry in `status.recordSets[]`, keyed by the
owner name as spelled in its own spec. A claimant that lost the election carries
`Programmed=False` with reason `NotOwner` on that entry, and a message saying
another record set owns the record.

That reason appears **per name**, never on the record set as a whole. A record
set that owns three names and lost two reports `Programmed=True` on three
entries and `NotOwner` on two.

`NotOwner` does not clear on its own. It persists for as long as both claims
stand, so it always means a person has to remove one of them. See
[Conditions and Reasons](./conditions.md) for the full taxonomy and for how the
per-name reasons relate to the record set's aggregate condition.

## Owner name spellings

The same name can be written several ways, and the backend and the election do
not treat those spellings alike. This is the part most likely to surprise.

### How a name is qualified

The backend keys its record set on an absolute name, derived from the spelling
in the spec plus the zone's domain:

| Spelling in `records[].name` | Zone `example.com` | Meaning |
|---|---|---|
| `@` | `example.com.` | The zone apex |
| `www` | `www.example.com.` | Relative to the zone |
| `www.example.com.` | `www.example.com.` | Already absolute, used as written |

A trailing dot means the name is absolute and the zone is not appended. Anything
else is a relative label and the zone is appended. `@` is the apex, and so is
the zone's own domain written out in full.

A name that ends in a dot is taken at its word. The operator does not check that
it falls inside the zone, so an absolute name belonging to some other zone is
qualified to itself and sent to the backend, which refuses it. That refusal
reaches the record set as a backend-error reason on that name.

### Where the election disagrees

The election compares the spelling as written in the spec, byte for byte. The
backend compares the qualified name. So `www` and `www.example.com.` in zone
`example.com` are **two separate elections** that resolve to **one backend
record set**.

Both claimants win their own key, and both write the same backend record set.
The later write replaces the earlier one, and neither reports `NotOwner`,
because as far as the election is concerned they never met.

One guard exists for the worst consequence of this. Before deleting a backend
record set, the agent checks whether any other spelling in the zone still
qualifies to the same name, and skips the delete if one does. This is what keeps
a rename from deleting the record the new spelling just wrote. It protects the
delete path only. It does not merge the claims, and it does not report the
collision anywhere.

### Case

DNS names are case-insensitive ([RFC 4343](https://www.rfc-editor.org/rfc/rfc4343)),
and the PowerDNS backend treats `WWW` and `www` as one name.

The operator does not fold case anywhere today. `WWW` and `www` are two
elections, they write one backend record set, and the aliasing guard above does
not recognise them as the same name either.

**The intended semantics are case-insensitive**, matching the backend. Two
spellings are the same claim when they qualify to the same absolute name after
case folding. Anything comparing owner names should implement that rule rather
than the current byte comparison:

1. Trim surrounding whitespace and lowercase the name and the zone.
2. Read `@`, and an empty name, as the zone apex.
3. Read a trailing dot as already absolute.
4. Otherwise append the zone.

The DNS command-line plugin in this repository already normalises this way when
it reads status, which is why it can report a conflict the operator's own
comparison misses.

> [!NOTE]
> Closing the gap on the write path is proposed in
> https://github.com/datum-cloud/dns-operator/pull/117, which refuses a claim on
> a name another record set already holds and matches every spelling
> case-insensitively.

### Advice for authors

Pick one spelling of a name and use it everywhere in a zone. Mixing `www`,
`WWW`, and `www.example.com.` across record sets produces writes that overwrite
each other with no conflict reported by either side.

## Related

- [API Reference](./api-reference.md#dnsrecordset) for the resource schema.
- [Conditions and Reasons](./conditions.md) for what each status reason means.
- [Replication Model](./replication.md) for how status reaches the tenant
  control plane.
