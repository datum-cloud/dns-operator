# Manage DNS with `datumctl dns`

The `dns` plugin for [`datumctl`](https://github.com/datum-cloud/datumctl) lets you create DNS zones and records on Datum Cloud from your terminal.

## Install the plugin

Install it from the official Datum plugin catalog, then confirm that `datumctl` found it:

```console
$ datumctl plugin install dns
Installed dns v0.6.8 from datum  [official]

$ datumctl dns version
datumctl-dns v0.6.8 (DNS API dns.networking.miloapis.com/v1alpha1)
```

Other plugin commands:

| Command | What it does |
|---|---|
| `datumctl plugin search dns` | Find the plugin and check its trust badge |
| `datumctl plugin list` | Show installed plugins, their versions, and status |
| `datumctl plugin upgrade dns` | Update to the latest release |
| `datumctl plugin remove dns` | Uninstall |

`datumctl dns version` needs no login, no project, and no network, so run it first whenever something else fails.

## Get started

```sh
datumctl dns zone create example.com                          # create a zone
datumctl dns zone nameservers example.com                     # what to set at your registrar
datumctl dns record create example.com www A 203.0.113.10     # add a record
datumctl dns record list example.com                          # see the whole zone
```

A zone holds the records for one domain. The domain doesn't resolve through Datum until you point it at Datum's nameservers at your registrar.

## Create a zone

```sh
datumctl dns zone create example.com
datumctl dns zone create example.com --description "production apex"
datumctl dns zone create example.com --no-wait
```

The command waits up to 2 minutes for Datum to assign nameservers, then prints them. Use `--no-wait` to return as soon as the zone exists, or `--timeout` to change how long it waits.

> [!WARNING]
> A zone's domain name can't be changed after you create it. To use a different domain, create a new zone.

## Point your domain at Datum

Datum assigns nameservers to your zone. Your registrar has to publish them before the domain resolves.

```console
$ datumctl dns zone nameservers example.com
Nameservers for example.com
  ns1.datum.net.   not set at registrar
  ns2.datum.net.   not set at registrar

Delegation   Incomplete — 0 of 2 nameservers set at the registrar

Set these nameservers at your domain registrar:
  ns1.datum.net.
  ns2.datum.net.

Currently delegated to:
  ns-cloud-a1.googledomains.com.
  ns-cloud-a2.googledomains.com.

Re-check with: datumctl dns zone nameservers example.com --check
```

Copy the nameservers into your registrar's control panel, then re-check. `--check` queries public DNS directly, so it tells you whether delegation is working rather than what Datum believes.

| Delegation state | Meaning |
|---|---|
| `Complete` | Your registrar publishes every assigned nameserver. |
| `Partial` | Your registrar publishes some of them — usually a half-finished edit. |
| `Incomplete` | Your registrar publishes none of them. |
| `Unknown` | There's nothing to compare yet. This is normal for the first few minutes after you create a zone. |

Registrar changes take time to propagate. Allow for the parent zone's TTL before you treat a change as failed.

## List and inspect zones

```console
$ datumctl dns zone list
NAME              STATUS    RECORDS   NAMESERVERS                      DELEGATED   AGE
example.com       OK        12        ns1.datum.net., ns2.datum.net.   yes         14d
old.acme.io       OK        8         ns1.datum.net., ns2.datum.net.   no          21d
staging.acme.io   Pending   2         —                                unknown     3m

3 zones — 2 OK, 1 Pending, 0 Rejected, 0 Error
```

Filter with `--status ok|pending|error`. Add `-o wide` for the zone's class and linked domain.

`datumctl dns zone describe example.com` adds a delegation report, a record count by type, and suggested next commands.

## Delete a zone

> [!WARNING]
> Deleting a zone deletes every record in it, and the domain stops resolving through Datum.

To confirm, type the zone name in full. Outside a terminal the command refuses to run unless you pass `--yes`:

```console
$ datumctl dns zone delete example.com
Error: refusing to perform a destructive action non-interactively without confirmation
Fix:   re-run with --yes to confirm "example.com".
exit status 9   # DNS_ABORTED
```

## Add a record

Give the zone, the name, the type, and one or more values. Names are relative to the zone: `www`, `*`, `_dmarc`, or `@` for the domain itself.

```console
$ datumctl dns record create example.com www A 203.0.113.10
  record/example.com A www created
  www  Auto  IN  A  203.0.113.10
```

`create` adds to what's already at that name. Run it again to add a second address:

```console
$ datumctl dns record create example.com www A 203.0.113.11 --ttl 5m
  record/example.com A www created
  www  5m  IN  A  203.0.113.10
  www  5m  IN  A  203.0.113.11
```

`set` replaces everything at that name instead:

```console
$ datumctl dns record set example.com www A 203.0.113.20
  record/example.com A www updated
  www  5m  IN  A  203.0.113.20
```

Add `--wait` to block until the record is live, or `--dry-run` to check a command without writing anything.

## Enter record values

Simple types take their value as a positional argument. Repeat the argument for multiple values:

```sh
datumctl dns record create example.com www A 203.0.113.10 203.0.113.11
datumctl dns record set    example.com @   TXT "v=spf1 include:_spf.example.com ~all"
datumctl dns record create example.com cdn CNAME lb.example.net.
```

Types with several parts take named flags:

```sh
datumctl dns record create example.com @ MX --preference 10 --exchange mail.example.com.
datumctl dns record create example.com _sip._tcp SRV --priority 10 --weight 5 --port 5060 --target sip.example.com.
datumctl dns record create example.com @ CAA --flag 0 --tag issue --value letsencrypt.org
datumctl dns record create example.com api HTTPS --priority 1 --target . --param alpn=h3,h2
```

| Type | Positional value | Named flags |
|---|---|---|
| `A`, `AAAA` | `<ip>` | — |
| `CNAME`, `ALIAS`, `NS`, `PTR` | `<hostname>` | — |
| `TXT` | `<string>` | `--data` |
| `MX` | `<preference> <exchange>` | `--preference --exchange` |
| `SRV` | `<priority> <weight> <port> <target>` | `--priority --weight --port --target` |
| `CAA` | `<flag> <tag> <value>` | `--flag --tag --value` |
| `TLSA` | `<usage> <selector> <matchingType> <certData>` | `--usage --selector --matching-type --cert-data` |
| `HTTPS`, `SVCB` | `<priority> <target> [k=v ...]` | `--priority --target --param k=v` |
| `SOA` | — | `--mname --rname --serial --refresh --retry --expire --minimum` |

Both notations work for every type, so you can paste a value straight out of a provider export or `dig` output:

```sh
datumctl dns record create example.com _sip._tcp SRV "10 5 5060 sipserver.example.com."
datumctl dns record create example.com --line "www 300 IN A 203.0.113.10"
```

For long TXT values, `--data` reads a file with `@path` or standard input with `-`:

```sh
datumctl dns record create example.com selector1._domainkey TXT --data @dkim.txt
dig +short TXT _dmarc.example.com | datumctl dns record set example.com _dmarc TXT --data -
```

Two rules catch most mistakes:

- **Names are relative.** Use `www`, not `www.example.com`. Use `@` for the domain itself.
- **Targets are absolute.** End every hostname inside a value with a dot: `mail.example.com.`, not `mail`.

## Set a TTL

`--ttl` takes seconds or a duration. The units are `s`, `m`, `h`, `d`, and `w`, and they combine: `--ttl 300`, `--ttl 5m`, `--ttl 1h30m`.

Omit `--ttl` and the record uses `Auto`, which resolves to 5 minutes. Displayed TTLs always carry a unit — `300` shows as `5m`, `3600` as `1h`, `86400` as `1d` — and every displayed value can be pasted back into `--ttl`. Values aren't rounded to preset options: `240` stays `240`.

## List and inspect records

One row per value:

```console
$ datumctl dns record list example.com
NAME     TYPE    TTL    VALUE                  STATUS
@        MX      Auto   10 mail.example.com.   Programmed
_dmarc   TXT     Auto   "v=DMARC1\; p=none"    Programmed
api      CNAME   Auto   lb.example.net.        Pending
www      A       5m     203.0.113.10           Programmed
www      A       5m     203.0.113.11           Programmed

5 records — 4 Programmed, 1 Pending
```

Filters: `--type A,MX`, `--name www`, and `--status programmed|pending|conflict|not-owner|error|rejected`.

Two more narrow by who writes the record: `--managed` shows the ones Datum manages for you, and `--no-managed` shows only your own. In a zone where another system writes most of the records, `--no-managed` is the view of what you actually put there. `--managed=false` means the same as `--no-managed`.

Long names and values are shortened to keep the table readable, and the counts are reported below it. Names are cut at 40 characters, which leaves the shapes people write — `www`, `_dmarc`, `_acme-challenge`, a DKIM selector — intact while shortening the encoded identifiers that automation writes. Use `-o wide` or `-o json` to see them in full, or `-o name` for the identifiers alone.

`datumctl dns record describe example.com @ MX` shows the value both as a single line and broken into named fields, plus the current status. Omit the type to see every record at that name.

> [!NOTE]
> A semicolon starts a comment in zone-file syntax, so TXT values containing one display escaped: `v=DMARC1; p=none` reads back as `"v=DMARC1\; p=none"`. The stored value is unchanged.

## Delete a record

Pass a value to remove only that value, or leave it off to remove everything at that name and type:

```console
$ datumctl dns record delete example.com www A --yes
  record/example.com A www deleted
  - www  5m  IN  A  203.0.113.20
  record set example-com-a removed — no A records remain in the zone
```

## Move a zone in from another provider

Import a zone file you exported from your old provider:

```sh
datumctl dns zone import example.com --file example.com.zone
datumctl dns zone import example.com --file example.com.zone --replace --dry-run
```

`--replace` overwrites the existing records of each type in the file instead of adding to them. `--dry-run` shows what would change without writing.

If you don't have a zone file, snapshot what the domain resolves to today:

```sh
datumctl dns zone import example.com --discover
```

TTLs are taken exactly as written in the file.

## Export and apply a zone file

Write the zone out in BIND format:

```console
$ datumctl dns zone export example.com
$ORIGIN example.com.
$TTL 300

; CNAME
api  IN CNAME lb.example.net.

; MX
@  IN MX 10 mail.example.com.
```

> [!NOTE]
> Two things a zone file can't carry, which `zone export` warns about on stderr. `ALIAS` isn't a standard record type, so other providers and BIND tooling reject those lines. And records Datum manages for you export as ordinary records, so importing the file elsewhere recreates them as yours.

`record apply` compares a zone file against the live zone and makes the zone match:

```console
$ datumctl dns record apply example.com -f example.com.zone --dry-run
  +   www    A       5m        203.0.113.10
  +   www    A       5m        203.0.113.99
  +   shop   CNAME   5m        shops.example.net.
  →   @      MX      5m → 1h   10 mail.example.com.

4 changes — 3 to add, 1 to change

Dry run — 4 changes validated, nothing was written.
```

Drop `--dry-run` to apply the changes. Re-running an unchanged file reports `No changes.`, which makes `apply --dry-run` usable as a drift check. By default `apply` only adds and updates; add `--prune` to also delete records the file doesn't mention.

Datum manages some records for you — the zone's SOA and apex NS records, and records created by a Gateway. `apply` never prunes or modifies them, and reports what it skipped.

## Output formats and scripting

Every command accepts `-o`:

| Format | Use it for |
|---|---|
| `table` | The default, for reading at a terminal. |
| `wide` | Adds columns. |
| `json`, `yaml` | Full API objects, for scripts. |
| `name` | Bare identifiers, for pipelines. |

```sh
datumctl dns zone list -o name | xargs -n1 datumctl dns zone describe
datumctl dns zone list -o json | jq -r '.items[].spec.domainName'
datumctl dns record list example.com -o name | cut -d/ -f1 | sort -u
```

`--no-headers` drops the header row from `table` and `wide` output. Data goes to standard output and everything else to standard error, so `-o json > file.json` is always clean.

> [!IMPORTANT]
> The table view is a presentation and its columns can change. Script against `-o json` or `-o name`.

Global flags: `--project`, `--org`, `-o/--output`, `-v/--verbose`, `-q/--quiet`, `-y/--yes`, and `--color auto|always|never`.

## Exit codes

Scripts can rely on these. A bulk operation that partly fails never exits 0.

| Code | Name | Meaning |
|---|---|---|
| 0 | — | Success |
| 1 | `DNS_ERROR` | Unexpected failure |
| 2 | `DNS_USAGE` | Bad flags, arguments, or record values |
| 3 | `DNS_FORBIDDEN` | Not authorized, or DNS isn't enabled for the project |
| 4 | `DNS_NOT_FOUND` | Zone or record not found |
| 5 | `DNS_CONFLICT` | Something else already owns that name |
| 6 | `DNS_INVALID` | The server rejected the request |
| 8 | `DNS_UNAVAILABLE` | Can't reach the DNS API |
| 9 | `DNS_ABORTED` | You declined a confirmation |

Errors print a problem, an optional fix, and the exit status:

```console
$ datumctl dns zone describe nope.example
Error: zone "nope.example" not found in project acme-prod
Fix:   list the zones in this project:
       datumctl dns zone list
exit status 4   # DNS_NOT_FOUND
```

Add `--verbose` to see the underlying cause.

## Troubleshoot

| Problem | What to do |
|---|---|
| No project set | Run `datumctl ctx use <org>/<project>`, or pass `--project <name>`. The plugin never guesses a project. |
| DNS is not enabled for the project | Run `datumctl services enable dns.networking.miloapis.com --wait`. |
| The domain doesn't resolve | Run `datumctl dns zone nameservers <domain> --check`. If delegation is `Incomplete` or `Partial`, fix it at your registrar. |
| A record is stuck at `Pending` | This is normal right after a write. If it lasts, run `datumctl dns record describe <domain> <name>` for the server's message. |
| `Conflict` | Another record occupies that name. Usually the name isn't inside the zone — check for `www.example.com` where `www` was meant. Otherwise see [DNS record conflict troubleshooting](../troubleshooting/dnsrecordset-downstream-orphan.md). |
| `Not owner` | Another record set owns that name; `describe` names it. Edit the record through that set, or delete the set first. Records Datum manages, such as a Gateway's, revert if you edit them. |

## See also

- [Architecture overview](../architecture/README.md)
- [API reference](../architecture/api-reference.md)
- [Troubleshooting runbooks](../troubleshooting/)
