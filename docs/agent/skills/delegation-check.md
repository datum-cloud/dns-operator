# Skill: delegation-check

Use when a zone is Accepted and Programmed but the domain still doesn't
resolve, or a customer asks whether their nameservers are set up right.
Call `dns_delegation_check` rather than reading nameservers off
`dns_zones_get` yourself — it compares each expected nameserver against
what was actually observed, one at a time, and that per-nameserver
detail is what tells a customer exactly which entry at their registrar
is missing.

## Procedure

1. Call `dns_delegation_check` for the zone.

2. Read `state`:
   - **Complete**: every nameserver Datum assigned is observed at the
     registrar. Delegation is not the problem — if the customer still
     reports an issue, look at resolver caching or a record that isn't
     programmed instead, and don't send them back to their registrar
     for this.
   - **Partial**: some but not all nameservers are set. Read the
     `nameservers` list and tell the customer exactly which ones are
     missing at their registrar — don't just say "some are missing,"
     name them.
   - **Incomplete**: none of the assigned nameservers are observed.
     Nothing at the registrar points at Datum yet. This is the
     customer's registrar configuration to fix, not something Datum can
     do for them.
   - **Unknown**: say plainly that this hasn't been checked, not that
     it's broken. Check `domainLinked`: if false, there's no Domain
     object to compare against at all (usually because domain
     verification hasn't completed — hand off to `domain-verification`
     if so); if true, the registrar simply hasn't been observed yet,
     which is normal for a few minutes after a zone is created.

3. Never infer delegation state from `dns_zones_get`'s `Programmed`
   condition. A zone can be fully programmed with default records and
   still have zero of its nameservers observed at the registrar — the
   two facts are independent, and conflating them is the single most
   common misattribution in DNS support.

## Do not

- Do not report Unknown as "not delegated" or "broken." It means the
  check hasn't run or has nothing to compare against yet.
- Do not tell a customer to fix delegation before confirming their zone
  is Accepted and Programmed first — `zone-not-resolving` sets that
  order for a reason.
- Do not guess which nameservers are missing from a summary count alone;
  always read the per-nameserver list and name them.
