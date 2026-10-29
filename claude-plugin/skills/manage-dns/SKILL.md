---
name: manage-dns
description: Use when someone wants to create, list, change, export or delete Datum DNS zones and records from their terminal or a script with datumctl. Covers the datumctl dns plugin commands and the rules that catch most mistakes.
---

# Manage Datum DNS with datumctl

Use this when the person wants to run the change themselves, script it, or
work from a terminal. To diagnose a zone or record, use the triage skills in
this plugin (start with `zone-not-resolving`), which read status through the
datum-mcp DNS tools.

The full command reference is
https://github.com/datum-cloud/dns-operator/blob/main/docs/cli/datumctl-dns.md.

## Before anything else

1. `datumctl dns version` needs no login or network. If it fails, the
   plugin isn't installed: `datumctl plugin install dns`.
2. The plugin never guesses a project. Set one with
   `datumctl ctx use <org>/<project>`, or pass `--project <name>`.
3. If a command exits 3 saying DNS isn't enabled, run
   `datumctl services enable dns.networking.miloapis.com --wait`.

## Common commands

```sh
datumctl dns zone create example.com                       # waits for nameservers
datumctl dns zone nameservers example.com --check          # what to set at the registrar, checked live
datumctl dns zone list
datumctl dns zone describe example.com

datumctl dns record create example.com www A 203.0.113.10  # adds to what's at www
datumctl dns record set    example.com www A 203.0.113.20  # replaces what's at www
datumctl dns record create example.com @ MX --preference 10 --exchange mail.example.com.
datumctl dns record list example.com --no-managed          # only the person's own records
datumctl dns record describe example.com www
datumctl dns record delete example.com www A --yes

datumctl dns zone export example.com > example.com.zone
datumctl dns record apply example.com -f example.com.zone --dry-run
```

`--ttl` takes seconds or a duration (`300`, `5m`, `1h30m`). Omit it for
`Auto`, which is 5 minutes.

## Rules that catch most mistakes

- Names are relative to the zone: `www`, not `www.example.com`. Use `@` for
  the domain itself. A `Conflict` right after a create is often this.
- Hostnames inside values are absolute and end with a dot:
  `mail.example.com.`.
- `create` adds a value, `set` replaces every value at that name and type.
  Say which one you are about to run.
- Run `--dry-run` first for anything that writes, show the person the
  result, and run it for real only after they say yes.
- A zone's domain name can't change after it is created.
- `zone delete` removes every record in the zone and the domain stops
  resolving through Datum. Confirm the exact zone name with the person
  before passing `--yes`.
- Records Datum manages (the zone's SOA and apex NS, a Gateway's records)
  revert if edited. `record apply --prune` leaves them alone. See
  `managed-record-refused` for where those edits belong.
- Datum DNS does not support DNSSEC. Never suggest creating `DS`, `DNSKEY`,
  `RRSIG`, `NSEC` or `NSEC3` records. Before anyone points a domain's
  nameservers at Datum, follow the DNSSEC gate in `zone-import`.
- Script against `-o json` or `-o name`, never the table output. Exit codes
  are stable: 4 not found, 5 conflict, 6 rejected, 9 declined.
