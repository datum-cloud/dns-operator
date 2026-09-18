# Skill: conflicting-record

Use when a record's status reports Conflict, usually with a message like
"a conflicting record already exists for this name." This reason covers
two situations that need completely different advice, and
`dns_record_diagnose` has already done the work of telling them apart —
read its answer instead of assuming from the reason code alone.

## Procedure

1. Call `dns_record_diagnose` for the zone, record set, and owner name.
   Read the `pattern` and `actionability` fields on the root cause
   rather than stopping at the reason string "Conflict."

2. If actionability is user: something the customer controls really is
   at that name already, most often a CNAME or ALIAS that can't coexist
   with another record type at the same owner name. Tell them plainly
   which record needs to move or go, and that the DNS backend only
   re-checks conflicts roughly every ten minutes, so nothing changing in
   the first few minutes after a fix is expected, not a sign the fix
   didn't work.

3. If actionability is platform (the diagnosis carries a pattern marking
   it as an orphaned backend record): tell the customer plainly that
   nothing in their project actually uses this name, so the record
   holding it was left behind by something outside their control, and
   that only Datum can find and remove it. Give them the exact record
   name and type to pass along; do not ask them to change anything on
   their side, because there's nothing there to change. Escalate this to
   Datum rather than suggesting a workaround.

## Do not

- Do not tell a customer to "just remove the conflicting record" when
  the diagnosis says nothing in their project holds the name. There is
  nothing on their side to remove, and searching their own records for
  something that isn't there wastes their time.
- Do not treat a conflict that persists for a few minutes after a fix as
  evidence the fix failed. The re-check interval is roughly ten minutes.
- Do not guess which of the two cases applies from the message text
  alone — always confirm with `dns_record_diagnose`, since the same
  message string covers both.
