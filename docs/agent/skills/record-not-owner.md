# Skill: record-not-owner

Use when `dns_record_diagnose` or `dns_records_list` reports `NotOwner`
for an owner name. This never clears on its own: another record set
already holds this exact (zone, record type, owner name) combination,
and one of the two has to change.

## Procedure

1. Call `dns_records_list` for the zone and find every row with the
   same owner name and record type as the one reporting `NotOwner`.
   Qualify and case-fold the name first — `record-not-programmed`
   covers why two different spellings can both claim to be the same
   name.

2. Identify which record set actually owns the name (the one reporting
   `Programmed`, not `NotOwner`) and which one lost.

3. Ask the customer which record set should survive. Common cases:
   - The losing one is a leftover from an earlier setup they meant to
     replace: delete it.
   - Both were created on purpose for different reasons: rename one to
     a different owner name, since a (zone, type, name) triple can only
     have one live owner.

4. If neither record set is under the customer's control (check
   provenance first — see `managed-record-refused`), this isn't a
   customer-side conflict at all; treat it as a conflict between two
   producers instead and escalate.

## Do not

- Do not tell the customer to "just wait" — `NotOwner` has no expected
  window and never clears without one side changing.
- Do not delete a record set without confirming which one should
  survive; both may be intentional, and this is the customer's decision,
  not the assistant's.
