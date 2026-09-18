# Skill: record-not-programmed

Use when a specific record isn't showing up, or a customer says a record
"looks right but isn't working," after `zone-not-resolving` has already
ruled out the zone itself and delegation. This skill is about one owner
name inside one DNSRecordSet.

## Procedure

1. Call `dns_records_get` for the record set, or `dns_record_diagnose`
   for the specific zone, record set, and owner name if you already know
   them. Always read `status.recordSets[]`, the per-name status — never
   the record set's top-level Programmed condition. The top-level
   condition only names the first blocked name alphabetically and folds
   every other problem into "and N more."

2. Match the owner name qualified and case-folded before concluding
   anything about it. "www", "WWW", and "www.example.com." are the same
   name to the DNS backend even though they look different as typed. If
   `dns_record_diagnose` reports more than one spelling with disagreeing
   results, that is itself the finding — see the spelling trap below
   before treating either result as the truth.

3. Branch on the reported reason:
   - Pending: normal, and clears on its own inside its expected window.
     If `dns_record_diagnose` reports it as stalled, treat it as stuck,
     not as "still working," and escalate with the details it gives you.
   - NotOwner: another record set already holds this exact name. This
     never clears by itself. Load `record-not-owner`.
   - Conflict: load `conflicting-record` — this reason covers two very
     different situations and the diagnose tool has already told you
     which one applies.
   - PDNSError: read the message. `dns_record_diagnose` has already
     classified it as user-actionable, transient, or a case of the zone
     itself still coming up; trust that classification over guessing
     from the reason code alone.
   - Anything else you don't recognize: treat it as needing attention,
     not as healthy, and say plainly that this is new to you rather than
     inventing an explanation.

## The spelling trap

If a record "looks right" in one view but serves stale or missing data
in another, suspect that two different spellings of the same owner name
were both submitted at some point. Ownership is decided by comparing the
spelling as typed, but the backend only tracks one value per fully
qualified name — so both submissions can look successful in their own
status while only the most recently written one is actually live. The
fix is picking one spelling and using it consistently; there's no way to
have both "coexist" correctly.

## Do not

- Do not read the record set's top-level Programmed condition as the
  status of a specific name. It hides every problem but the
  alphabetically first one.
- Do not compare owner names as raw strings. Two spellings of one name
  are the same record to the DNS backend.
- Do not call a long-Pending record "still working" once it's outside
  its expected window — say it's stuck and give the customer or Datum
  the concrete duration to act on.
