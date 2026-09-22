# Skill: zone-import

Use when a customer wants to move a domain they already run elsewhere
onto Datum's DNS, and needs to recreate what it currently serves rather
than starting from nothing.

## Procedure

1. Call `dns_zone_discovery_get` for the zone's discovery snapshot.
   Check the `Discovered` condition first: if it isn't True yet, say
   the snapshot is still in progress and don't treat the record sets
   it lists as the complete picture.

2. State plainly that this is a one-time snapshot, not a live view of
   the domain. If real time has passed since it was taken, or the
   customer has changed something at their current provider since,
   offer to have a fresh one taken rather than working from a stale
   one.

3. Before recreating anything, ask whether the domain has DNSSEC
   turned on at its current provider. If it does, it has to come off
   there, and any DS records at the parent have to clear, before this
   zone is ever delegated to Datum's nameservers. Getting this order
   wrong takes the domain dark behind every validating resolver, and
   the failure looks nothing like a DNS problem from the customer's
   side — treat this as a hard gate, not a suggestion.

4. Walk the discovered record sets with the customer and confirm each
   one before recreating it. For anything more than a couple of
   simple records, lower TTLs on the current provider first and let
   the old ones expire before cutting over — this shortens how long a
   mistake stays cached if something needs to be redone.

5. Use `dns_record_render` for each record to recreate, the same way
   `record-create` does, reading its warnings and notes before handing
   anything to `resources_plan`.

6. Only once every record is recreated and confirmed with
   `dns_records_list` should the customer repoint their registrar at
   Datum's nameservers. Use `dns_delegation_check` afterward to confirm
   the cutover actually took.

## Do not

- Do not recreate records before confirming DNSSEC is off. This is the
  single most damaging mistake in a zone cutover and it is silent
  until a resolver that validates hits it.
- Do not repoint nameservers before every record is recreated and
  verified. Once delegation completes, the old provider stops serving
  answers for this domain even if Datum's records aren't ready yet.
- Do not treat a discovery snapshot as current once meaningful time has
  passed. Offer a fresh one rather than importing stale data.
