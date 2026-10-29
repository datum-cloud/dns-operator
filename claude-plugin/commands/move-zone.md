---
description: Move a domain's DNS from another provider onto Datum Cloud, step by step
argument-hint: <domain> [zone-file]
---

Move the DNS for `$1` onto Datum Cloud. If a second argument is given, it is a
BIND zone file exported from the current provider: `$2`.

Follow the `zone-import` skill as the source of truth for order and safety,
and stop for the person's yes before every step that writes. Work through
these steps, reporting each result before moving on:

1. Check the tools. Run `datumctl dns version` and `datumctl ctx` to confirm
   the dns plugin is installed and a project is selected. If either is
   missing, say how to fix it and stop.

2. Find or create the zone. Run `datumctl dns zone describe $1`. If it isn't
   found, offer `datumctl dns zone create $1`. If the zone reports
   PendingDomainVerification, follow `domain-verification`; records can be
   staged, but nothing goes live until it clears.

3. Check DNSSEC. Ask the person to run `dig +short DS $1`, or run it if a
   shell is available. If it prints anything, stop and walk them through the
   DNSSEC steps in `zone-import` before going further. This is a hard gate.

4. Preview the import. With a zone file, run
   `datumctl dns zone import $1 --file <zone-file> --dry-run`. Without one,
   run `datumctl dns zone import $1 --discover --dry-run`, and say that it
   is a snapshot of what the domain serves today. Show the person the
   summary, including every record it skips and why.

5. Import. After the person says yes, run the same command without
   `--dry-run`. Then run `datumctl dns record list $1` and go through it with
   them. Any record not `Programmed` goes to `record-not-programmed`.

6. Cut over. Only once every record is confirmed, run
   `datumctl dns zone nameservers $1` and tell the person exactly which
   nameservers to set at their registrar. Suggest lowering TTLs at the old
   provider first if they haven't.

7. Confirm. After they change the registrar, run
   `datumctl dns zone nameservers $1 --check` (or `dns_delegation_check`)
   and follow `delegation-check` for anything short of Complete.
