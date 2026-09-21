# Skill: managed-record-refused

Use when a customer wants to edit, delete, or asks why they can't
change a record, and `dns_records_list` or `dns_records_get` reports its
provenance as anything other than user. Editing here doesn't stick:
whatever owns the record puts its own value back the next time it
checks, so telling a customer "done" would be reporting a change that
quietly disappears.

## Procedure

1. Read the record's `provenance` field from `dns_records_list` or
   `dns_records_get`, and its `provenanceSource` if one is present.

2. Tell the customer plainly, by provenance:
   - **gateway**: this record is managed by Datum's AI Edge, tied to the
     Gateway or route named in `provenanceSource`. The real edit belongs
     on that Gateway object, in whatever surface they manage it through
     — not on the DNS record directly.
   - **iroh**: this record supports one of their connector endpoints,
     tied to the connector named in `provenanceSource`. Changing the
     record won't change the connector's behavior; if something about
     the connector needs to change, that happens on the connector
     itself.
   - **external-dns**: an external-dns integration manages this record,
     tied to the source named in `provenanceSource` when one is given.
     The edit belongs in whatever system that integration syncs from.
   - **platform**: this is the zone's own SOA record, or its apex NS
     records. Datum created it and depends on it, but the API does
     allow editing it — say what breaks if they do (SOA: zone transfers
     and negative caching; apex NS: delegation) rather than refusing
     outright, since this tier is a warning, not a hard block.

3. In every managed case except platform, be direct that no retry or
   different wording will make the edit stick — the fix is on the
   owning object, full stop.

## Do not

- Do not suggest deleting and recreating a managed record as a
  workaround. Whatever owns it writes it back, so this produces a brief
  gap in service and no lasting change.
- Do not treat platform-tier records the same as gateway, iroh, or
  external-dns. Those three are reverted; platform-tier records are
  merely risky to touch, and the customer is allowed to proceed with a
  clear warning.
- Do not guess an owner from the reason a record failed to program.
  Provenance comes from the labels the tools already report — read
  those, don't infer.
