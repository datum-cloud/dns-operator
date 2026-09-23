# Skill: record-create

Use when a customer wants to add a DNS record. This is the only place
in this assistant's DNS coverage that ends in a write, and even here
the write is the person's own decision at the end, not this assistant's
— `dns_record_render` only builds a manifest for `resources_plan` and
`resources_apply` to carry out once they say yes.

## Procedure

1. Gather what the record needs before rendering anything: which zone,
   the owner name, the record type, and the value for that type (a
   content string for A/AAAA/CNAME/NS/TXT, or the structured fields for
   MX/SRV/CAA). Ask rather than guess — a wrong value renders cleanly
   and fails only once applied.

2. Call `dns_records_list` for the zone first and check whether the
   owner name is already claimed by another record set of the same
   type. If it is, this isn't a create at all: hand off to whichever of
   `record-not-owner` or `conflicting-record` actually describes what's
   there, or tell the customer their new record will collide.

3. Call `dns_record_render` with the gathered details. Read every
   warning and note it returns:
   - A CNAME warning about the apex or about coexistence means the
     record as described will likely be rejected — resolve it with the
     customer before going further, don't render around it.
   - A note that a record set of this type already exists in the zone
     means the manifest render produced is not what should be applied
     — that owner name has to be added to the existing object's
     `records` list instead of creating a second one. Read the
     existing set with `dns_records_get` and combine them yourself
     before handing anything to `resources_plan`.
   - The TTL note states what will actually apply. State it to the
     customer before they confirm, especially when it defaulted rather
     than being asked for.

4. Hand the (possibly merged) manifest to `resources_plan`, then
   `resources_apply` once the customer agrees. Never call apply without
   an explicit yes on the specific manifest shown.

## Do not

- Do not skip the `dns_records_list` check in step 2. `dns_record_render`
  only reads the zone's own record sets for its coexistence note; it
  does not check per-owner-name status, and skipping this step is how
  a "successful" render turns into a rejected apply.
- Do not apply a rendered manifest as-is when its notes say a same-type
  record set already exists. Applying it anyway creates a conflicting
  object rather than adding the record.
- Do not silently drop a warning because the customer seems confident.
  State it, then let them decide.
