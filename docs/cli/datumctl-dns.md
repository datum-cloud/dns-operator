# `datumctl dns` — manage DNS zones and records

`datumctl-dns` is a first-party [`datumctl`](https://github.com/datum-cloud/datumctl) plugin for managing Datum Cloud DNS from the terminal. It presents **records**, not record sets: the `(zone, type)` bucketing the API stores is an implementation detail the plugin hides on read and reconstructs on write, so you can add an A record without knowing that `DNSRecordSet` exists.

## Contents

- [Installation](#installation)
- [Prerequisites](#prerequisites)
- [Quick start](#quick-start)
- [`version`](#version)
- [Zone commands](#zone-commands)
- [Record commands](#record-commands)
- [Record grammar](#record-grammar)
- [Output and scripting](#output-and-scripting)
- [Exit codes](#exit-codes)
- [Troubleshooting](#troubleshooting)

## Installation

### From source

This is the only install path that works today.

```sh
git clone https://github.com/datum-cloud/dns-operator
cd dns-operator
make install-plugin
```

`make build-plugin` compiles the binary to `./bin/datumctl-dns`, and `make install-plugin` builds it and copies it to `~/.datumctl/plugins/datumctl-dns`. Verify that `datumctl` found it:

```console
$ datumctl dns version
datumctl-dns v0.6.8 (DNS API dns.networking.miloapis.com/v1alpha1)
```

This needs no login, no project, and no network, so it is also the first thing to run when something else is not working. (`datumctl dns --plugin-manifest` answers a similar question, but it is the machine hook the host uses to enumerate plugins, not a command meant for people.)

> [!IMPORTANT]
> The binary must be named exactly `datumctl-dns`. `datumctl` dispatches `datumctl dns ...` by looking for a file with that name in its plugins directory; a renamed or versioned filename will not be found.

### Installing somewhere else

`datumctl` resolves its plugins directory from `DATUMCTL_PLUGINS_DIR`, falling back to `~/.datumctl/plugins`. The Makefile's install destination is a separate variable, `DATUMCTL_PLUGIN_DIR` (singular), so overriding the location means setting **both**:

```sh
make install-plugin DATUMCTL_PLUGIN_DIR=/tmp/p
DATUMCTL_PLUGINS_DIR=/tmp/p datumctl dns --help
```

### Forthcoming install paths

Two further paths are configured in this repository but **not yet exercised** — do not follow them expecting them to work:

- **GitHub release archives.** [`.goreleaser-plugin.yaml`](../../.goreleaser-plugin.yaml) builds cross-platform archives, but no release has been published yet.
- **`datumctl plugin install dns`.** [`hack/datumctl-plugin/dns.yaml`](../../hack/datumctl-plugin/dns.yaml) is a draft curated-index entry. Nothing in this repository consumes it; it resolves only once it is merged into `datum-cloud/datumctl-plugins` as `plugins/dns.yaml`, which requires a published release first. Its `sha256` values are placeholder zeros.

## Prerequisites

Three things, in order:

```sh
datumctl login                                            # authenticate
datumctl ctx use <org>/<project>                          # select a project
datumctl services enable dns.networking.miloapis.com      # enable DNS
```

Enabling DNS is **self-service**: the entitlement is created and reaches `Active` immediately, with no approval step. Pass `--wait` if you want the command to block until it is active.

> [!NOTE]
> The plugin runs this check itself before every command that touches the API, and will offer to enable DNS for you if it is not enabled and you are at a terminal. In a non-interactive session it refuses with exit code 3 rather than hanging on a prompt nobody can answer. `version`, `completion`, `help`, and the shell-completion hooks skip the check entirely.

The service is named two different ways, and both are correct in their place: `dns.networking.miloapis.com` is the **service identifier** you type, and `dns-networking-miloapis-com` is the entitlement object's name. You only ever need the first.

## Quick start

```sh
datumctl dns zone create example.com                          # create a zone, wait for nameservers
datumctl dns zone nameservers example.com                     # what to set at the registrar
datumctl dns record create example.com www A 203.0.113.10     # add a record
datumctl dns record list example.com                          # see everything in the zone
```

A zone is not usable until the registrar points at Datum's nameservers, so `zone create` waits for them to be assigned and prints them. That delegation step happens at your domain registrar, not in Datum.

## `version`

```console
$ datumctl dns version
datumctl-dns v0.6.8 (DNS API dns.networking.miloapis.com/v1alpha1)

$ datumctl dns version -o wide
datumctl-dns v0.6.8 (DNS API dns.networking.miloapis.com/v1alpha1)
  plugin API 1
  go1.26.1 darwin/arm64
```

`version` runs entirely offline: no credentials, no API call, no project, and no service entitlement. That is deliberate — a version check is something you reach for while debugging a broken login or an unreachable control plane, and one that needs either of those is useless exactly when you want it. `-o json` and `-o yaml` emit the same fields for scripts.

## Zone commands

```
datumctl dns zone list        [--status ok|pending|error] [--no-headers] [-o table|wide|json|yaml|name]
datumctl dns zone create      <domain> [--class <name>] [--description <text>] [--no-wait] [--timeout <d>] [--dry-run]
datumctl dns zone describe    <domain> [-o wide|json|yaml]
datumctl dns zone nameservers <domain> [--check] [--timeout <d>]
datumctl dns zone delete      <domain> [--yes] [--dry-run]
datumctl dns zone import      <domain> --file <zonefile> | --discover [--replace] [--dry-run] [--timeout <d>]
datumctl dns zone export      <domain> [--file <path>]
```

Aliases: `zone` is also `zones` and `z`; `list` is `ls`; `describe` is `show` and `get`; `delete` is `rm`; `nameservers` is `ns`. Running the group bare (`datumctl dns zone`) is the same as `zone list`.

### `zone list`

```console
$ datumctl dns zone list
NAME              STATUS    RECORDS   NAMESERVERS                      DELEGATED   AGE
example.com       OK        12        ns1.datum.net., ns2.datum.net.   yes         14d
old.acme.io       OK        8         ns1.datum.net., ns2.datum.net.   no          21d
staging.acme.io   Pending   2         —                                unknown     3m

3 zones — 2 OK, 1 Pending, 0 Error
```

A project with no zones gets a starting point rather than a blank table:

```console
$ datumctl dns zone list
No DNS zones found in project acme-prod.

Get started:
  datumctl dns zone create example.com
```

`DELEGATED` is `unknown` when there is nothing to compare — a zone with no nameservers assigned yet, or no linked `Domain` object. The footer tally is computed **after** filtering, so `--status pending` reports the count of what it printed, not of the project.

`-o wide` appends `CLASS` and `DOMAIN`:

```console
$ datumctl dns zone list -o wide
NAME              STATUS    RECORDS   NAMESERVERS                      DELEGATED   AGE   CLASS                       DOMAIN
example.com       OK        12        ns1.datum.net., ns2.datum.net.   yes         14d   datum-external-global-dns   example-com
```

### `zone create`

```sh
datumctl dns zone create example.com                                          # waits for nameservers
datumctl dns zone create example.com --dry-run                                # validate, create nothing
datumctl dns zone create example.com --no-wait --description "production apex"
```

The command **waits by default** (up to `--timeout`, default 2m) because a zone is useless until it has nameservers and you cannot delegate the domain without knowing them. `--class` defaults to `datum-external-global-dns`, which is currently the only `DNSZoneClass` that exists.

> [!WARNING]
> A zone's domain name is immutable. There is no `zone update`; changing the domain means creating a new zone.

### `zone describe`

The delegation block is the useful part, and it changes shape depending on whether delegation is done. When it is:

```console
$ datumctl dns zone describe example.com
Zone         example.com                      project: acme-prod
Class        datum-external-global-dns
Created      14d ago

Status       OK — zone programmed, 12 records live
Delegation   Complete — all 2 nameservers set at the registrar

Nameservers
  ns1.datum.net.   set at registrar
  ns2.datum.net.   set at registrar

Records      12 across 6 types
  SOA 1    NS 2    A 3    CNAME 1    MX 1    TXT 4

Next steps:
  List records:            datumctl dns record list example.com
  Add a record:            datumctl dns record create example.com www A 203.0.113.10
  Export as a zone file:   datumctl dns zone export example.com
```

When it is not — the common first-run state — the block becomes the instruction:

```console
$ datumctl dns zone describe old.acme.io
...
Delegation   Incomplete — 0 of 2 nameservers set at the registrar

Nameservers
  ns1.datum.net.   not set at registrar
  ns2.datum.net.   not set at registrar

Records      8 across 3 types
  A 5    MX 1    TXT 2

Set these nameservers at your domain registrar:
  ns1.datum.net.
  ns2.datum.net.

Currently delegated to:
  ns-cloud-a1.googledomains.com.
  ns-cloud-a2.googledomains.com.

Re-check with: datumctl dns zone nameservers old.acme.io --check
```

Delegation state is computed by comparing the nameservers Datum assigned against the ones your registrar publishes, normalised lowercase with trailing dots stripped.

| State | Meaning |
|---|---|
| `Complete` | every assigned nameserver is published by the registrar |
| `Partial` | some but not all are — usually a half-finished edit at the registrar |
| `Incomplete` | the registrar was checked and publishes none of them |
| `Unknown` | there is nothing to compare yet: no nameservers assigned, no linked domain, or the registrar has not been checked |

> [!NOTE]
> `Unknown` is not a weaker `Incomplete`. `Incomplete` means the registrar was looked at and is pointing elsewhere; `Unknown` means nobody has looked yet, which is the ordinary state for the first minutes after a zone is created. The distinction matters because only one of them is a reason to go and change something at your registrar.

The record summary reconciles two independently maintained numbers — the per-type counts from the zone's record sets, and `status.recordCount` on the zone itself. When they disagree, it says so rather than printing figures that do not add up:

```
Records      12 across 4 types
  A 3    CNAME 1    MX 1    TXT 4
  the per-type counts add up to 9, not the 12 the zone reports — the operator is still catching up
```

When no record sets come back at all, it explains why there is no breakdown instead of printing a bare number.

### `zone nameservers`

Prints the same delegation block on its own. `--check` additionally queries the assigned nameservers directly and asks public DNS what the domain currently delegates to, which answers "is it actually working" rather than "is the control plane happy".

### `zone delete`

> [!WARNING]
> Deleting a zone **cascades**. The operator owns every record set in the zone through a controller `ownerReference`, so all of the zone's records are garbage-collected with it and the domain stops resolving.

Confirmation requires typing the zone name in full, and the command refuses to run non-interactively without `--yes`:

```console
$ datumctl dns zone delete example.com
Error: refusing to perform a destructive action non-interactively without confirmation
Fix:   re-run with --yes to confirm "example.com".
exit status 9   # DNS_ABORTED
```

### `zone import` and `zone export`

`export` flattens every record set in the zone into a BIND zone file, grouped by type:

```console
$ datumctl dns zone export example.com
$ORIGIN example.com.
$TTL 300

; CNAME
api  IN CNAME lb.example.net.

; MX
@  IN MX 10 mail.example.com.

; TXT
_dmarc  IN TXT "v=DMARC1\; p=none"
```

`import` bulk-loads one back. Records are grouped by type before writing, so each record type costs one API call regardless of how many records it holds.

```sh
datumctl dns zone export example.com --file example.com.zone
datumctl dns record apply example.com -f example.com.zone      # export → edit → apply
datumctl dns zone import example.com --file example.com.zone --replace
datumctl dns zone import example.com --discover                # snapshot what the domain resolves to today
```

`--discover` creates a `DNSZoneDiscovery` and imports what the domain currently resolves to — the "migrate my zone off the old provider" path. `--replace` replaces the existing records of each type present in the input instead of merging into them. TTLs are taken from the file as written; unlike the portal, nothing is rounded onto a preset ladder.

## Record commands

```
datumctl dns record list     <domain> [--type A,MX] [--name www] [--status <word>] [--managed] [--no-headers] [-o ...]
datumctl dns record create   <domain> <name> <TYPE> [<value>...] [--ttl <t>] [--wait] [--force] [--dry-run]
datumctl dns record set      <domain> <name> <TYPE> [<value>...] [--ttl <t>] [--wait] [--force] [--dry-run]
datumctl dns record delete   <domain> <name> <TYPE> [<value>] [--yes] [--force] [--dry-run]
datumctl dns record describe <domain> <name> [<TYPE>]
datumctl dns record apply    <domain> -f <zonefile> [--prune] [--dry-run]
```

Aliases: `record` is also `records` and `rr`; `list` is `ls`; `describe` is `show` and `get`; `delete` is `rm`.

### `create` vs `set`

`create` **appends**: the values already at that `(name, type)` stay, and an exact duplicate is refused. `set` **replaces**: every value at that name is removed and the ones given take their place.

These are separate verbs because "add a second A record" and "change my A record" are different intents, and a single command cannot express both safely.

```console
$ datumctl dns record create example.com www A 203.0.113.10
  record/example.com A www created
  www  Auto  IN  A  203.0.113.10

$ datumctl dns record create example.com www A 203.0.113.11 --ttl 300
  record/example.com A www created
  www  300  IN  A  203.0.113.10
  www  300  IN  A  203.0.113.11

$ datumctl dns record set example.com www A 203.0.113.20
  record/example.com A www updated
  www  300  IN  A  203.0.113.20
```

A mutation driven by named flags confirms in presentation format, and `describe` on a record entered as presentation format shows the named fields. Each use teaches the other notation.

The zone is always the first positional argument. There is no default and no "last zone you used": a mistyped zone that silently resolves to something is how people delete production records.

### `record list`

One row per **value**, flattened from the zone's `DNSRecordSet` objects and sorted by name, then type, then value:

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

An empty zone does the same:

```console
$ datumctl dns record list example.com
No records found in zone example.com.

Get started:
  Add a record:   datumctl dns record create example.com www A 203.0.113.10
  Import a zone:  datumctl dns zone import example.com --file zone.txt
```

`STATUS` is the **per-owner-name** condition, never the rolled-up one on the record set: the interesting outcomes — `Conflict`, `Not owner`, `Error` — only exist per name, and the rollup flattens all of them to a generic `Pending`.

`--status` filters on the first word of the status, lowercased: `programmed`, `pending`, `conflict`, `not` (for `Not owner`), `error`, or `rejected`. `--type` takes a comma-separated list, `--name` filters to one owner name, and `--managed` shows only platform- and Gateway-managed records.

`-o name` emits `name/TYPE` pairs, which is what you want for piping:

```console
$ datumctl dns record list example.com -o name
@/MX
_dmarc/TXT
api/CNAME
www/A
```

### `record describe`

Shows the values both ways — presentation format, and broken out into the named fields — plus the backend's own status sentence, verbatim:

```console
$ datumctl dns record describe example.com @ MX
Record        example.com
Zone          example.com
Type          MX
TTL           Auto (5m)
Record set    example-com-mx
Created       2m ago

Values
  10 mail.example.com.
      Preference:  10
      Exchange:    mail.example.com.

Status        Programmed

Next steps:
  Change the value:    datumctl dns record set example.com @ MX <value>
  Add another value:   datumctl dns record create example.com @ MX <value>
  Remove it:           datumctl dns record delete example.com @ MX
  See the whole zone:  datumctl dns record list example.com
```

Omit the type to see every type at that name.

### `record delete`

With a value, only that value is removed; without one, every value at that `(name, type)` goes. The prompt says how many, so the difference is never a surprise.

```console
$ datumctl dns record delete example.com www A --yes
  record/example.com A www deleted
  - www  300  IN  A  203.0.113.20
  record set example-com-a removed — no A records remain in the zone
```

When the last value of a type leaves a zone, the `DNSRecordSet` holding it is deleted rather than left empty — the API requires at least one entry, so an empty set is not a writable state.

> [!NOTE]
> `record delete` prompts `y/N` at a terminal but **proceeds without prompting** when stdin is not a terminal, because the prompt cannot be answered there and a single record deletion is recoverable. `zone delete` is the opposite: it refuses non-interactively without `--yes`. The two tiers are scaled to blast radius.

### `record apply`

The declarative path: diff a zone file against the live zone, print what would change, and converge.

```console
$ datumctl dns record apply example.com -f example.com.zone --dry-run
  +   www    A       5m          203.0.113.10
  +   www    A       5m          203.0.113.99
  +   shop   CNAME   5m          shops.example.net.
  →   @      MX      Auto → 5m   10 mail.example.com.

4 changes — 3 to add, 1 to change

Dry run — 4 changes validated, nothing was written.
```

Drop `--dry-run` to apply, and re-running an unchanged file reports `No changes.` By default apply only adds and updates; `--prune` also deletes the records the file does not mention. Platform-managed records — the zone's SOA, its apex NS records, and anything owned by AI Edge — are never pruned or modified, and whatever was skipped is reported.

> [!WARNING]
> A record left at the default `Auto` TTL does not currently survive an `export` → `apply` round trip as a no-op. `zone export` writes `$TTL 300` and omits the per-record TTL for such records, so re-reading the file resolves them to an explicit `300` and the diff reports `Auto → 5m` for each one. Give records an explicit `--ttl` if you rely on `apply` being idempotent, for example in a drift check.

### Platform-managed records

The operator creates a `<zone>-soa` and a `<zone>-ns` record set for every zone, and the Gateway controller creates record sets labelled `dns.datumapis.com/source-kind: Gateway`. `record list` marks both categories, and `--managed` shows only those.

Gateway-owned records are effectively read-only: editing them fights a controller that will revert the change. SOA and apex-NS records are warned about rather than blocked, and `--force` permits the edit.

> [!WARNING]
> Editing apex NS records can break delegation. `--force` exists because the API allows the edit and the operator never reconciles their content, not because it is safe.

## Record grammar

Two notations. **Flat types are positional**, one value per argument, and repeating the argument makes a multi-value RRset:

```sh
datumctl dns record create example.com www A 203.0.113.10 203.0.113.11 --ttl 300
datumctl dns record set    example.com @   TXT "v=spf1 include:_spf.example.com ~all"
datumctl dns record create example.com cdn CNAME lb.example.net.
```

**Structured types take named flags**, because `"10 5 5060 sipserver.example.com."` is unreadable six months later:

```sh
datumctl dns record create example.com @ MX --preference 10 --exchange mail.example.com.
datumctl dns record create example.com _sip._tcp SRV --priority 10 --weight 5 --port 5060 --target sip.example.com.
datumctl dns record create example.com @ CAA --flag 0 --tag issue --value letsencrypt.org
datumctl dns record create example.com api HTTPS --priority 1 --target . --param alpn=h3,h2 --param port=443
```

| Type | Presentation grammar | Named flags |
|---|---|---|
| `A` / `AAAA` | `<ip>` | positional only |
| `CNAME` / `ALIAS` / `NS` / `PTR` | `<hostname>` | positional only |
| `TXT` | `<string>` | `--data` |
| `MX` | `<preference> <exchange>` | `--preference --exchange` |
| `SRV` | `<priority> <weight> <port> <target>` | `--priority --weight --port --target` |
| `CAA` | `<flag> <tag> <value>` | `--flag --tag --value` |
| `TLSA` | `<usage> <selector> <matchingType> <certData>` | `--usage --selector --matching-type --cert-data` |
| `HTTPS` / `SVCB` | `<priority> <target> [k=v ...]` | `--priority --target --param k=v` |
| `SOA` | — | `--mname --rname --serial --refresh --retry --expire --minimum` |

**Presentation format parses for every type**, so a value pasted from a provider export or a `dig` output works without translation:

```sh
datumctl dns record create example.com _sip._tcp SRV "10 5 5060 sipserver.example.com."
datumctl dns record create example.com @ CAA '0 issue "letsencrypt.org"'
```

Mixing positional rdata and named flags for the same value is a usage error, not a merge.

**`--line`** takes a whole `dig`-shaped line and parses name, TTL, type, and rdata out of it:

```sh
datumctl dns record create example.com --line "www 300 IN A 203.0.113.10"
```

**TXT** additionally accepts `--data @path/to/file` and stdin (`--data -`), because SPF and DKIM values are where shell quoting bites hardest:

```sh
datumctl dns record create example.com selector1._domainkey TXT --data @dkim.txt
dig +short TXT _dmarc.example.com | datumctl dns record set example.com _dmarc TXT --data -
```

Values over 255 characters are chunked into multiple quoted strings on write, per RFC 1035.

> [!NOTE]
> A semicolon starts a comment in zone-file format, so TXT values containing one are emitted escaped: `"v=DMARC1; p=none"` reads back as `"v=DMARC1\; p=none"` in `record list` and in an exported zone file. The stored value is unchanged; only the presentation is escaped.

### Names and the trailing dot

Owner names are **always zone-relative**. `@` is the apex; `www`, `*`, `_dmarc`, and `_sip._tcp` are labels. A name that already includes the zone domain is rejected rather than silently qualified, because the backend would turn `www.example.com` in zone `example.com` into `www.example.com.example.com.`:

```console
$ datumctl dns record create example.com www.example.com A 203.0.113.10
Error: record name "www.example.com" already includes the zone domain
Fix:   names are relative to the zone — use "www", or "www.example.com." with a trailing dot to force an absolute name
exit status 2   # DNS_USAGE
```

Target fields inside rdata are the opposite: the backend absolutises them by appending a single dot, so a relative `mail` becomes the root-relative `mail.` — almost certainly not what you meant. **Every target field requires a trailing dot:**

```console
$ datumctl dns record create example.com @ MX --preference 10 --exchange mail
Error: MX exchange "mail" is not a fully qualified domain name
Fix:   targets are absolute, not zone-relative — did you mean "mail.example.com."?
exit status 2   # DNS_USAGE
```

Per-field syntax is checked before anything is submitted, because the API server admits a malformed record and the backend then skips it silently:

```console
$ datumctl dns record create example.com www A not-an-ip
Error: "not-an-ip" is not a valid IPv4 address
Fix:   an A record holds a single IPv4 address, as in "203.0.113.10"
exit status 2   # DNS_USAGE
```

### TTL

`--ttl` takes seconds or a duration: `--ttl 300`, `--ttl 5m`, `--ttl 1h`, `--ttl 1d`. The units are `s`, `m`, `h`, `d` and `w`, and they compose (`1h30m`). Omitting it means `Auto`, the backend default of 5m, rendered as `Auto (5m)` in describe views so the number is never a mystery. Arbitrary TTLs are **not** snapped onto a preset ladder — `240` stays `240`.

TTLs are always **displayed** with their unit, never as a bare number: a `5` in a TTL column cannot be read without guessing at seconds versus minutes. The largest unit that divides evenly wins, so `300` shows as `5m`, `3600` as `1h` and `86400` as `1d`; a value that divides evenly into nothing larger stays in seconds (`90s`). Every rendered TTL parses back to the same number, so a value read off `record list` can be pasted straight into `--ttl`.

> [!NOTE]
> TTL is per-RRset in DNS but per-entry in the API, and the backend takes the first entry's TTL for an owner name and ignores the rest. The plugin applies `--ttl` to every value it writes for that `(name, type)`, and warns when it reads a set whose entries disagree.

## Output and scripting

Every command takes `-o`:

| Format | Use |
|---|---|
| `table` | the default, for a person at a terminal |
| `wide` | adds columns |
| `json` / `yaml` | the **raw API objects**, not the flattened rows |
| `name` | bare identifiers, for `xargs` and command substitution |

`--no-headers` omits the table header row (`table` and `wide` only), which is what you want when piping into `awk`:

```console
$ datumctl dns zone list --no-headers
example.com       OK        12   ns1.datum.net., ns2.datum.net.   yes       14d
old.acme.io       OK        8    ns1.datum.net., ns2.datum.net.   no        21d
```

```console
$ datumctl dns zone list -o json
{
  "kind": "DNSZoneList",
  "apiVersion": "dns.networking.miloapis.com/v1alpha1",
  "metadata": {
    "resourceVersion": "378"
  },
  "items": []
}
```

```sh
datumctl dns zone list -o name | xargs -n1 datumctl dns zone describe
datumctl dns zone list -o json | jq -r '.items[].spec.domainName'
datumctl dns record list example.com -o name | cut -d/ -f1 | sort -u
```

> [!IMPORTANT]
> `-o json` and `-o yaml` emit the raw `DNSZone` / `DNSRecordSet` objects. The flattened record view is a presentation, not a data contract — script against the raw objects or against `-o name`, not against the table.

Data goes to stdout and diagnostics, prompts, and errors go to stderr, so `-o json > file.json` is always clean.

Other flags worth knowing: `--verbose` adds the underlying cause to error output, `--quiet` suppresses footers and progress, `--yes` skips confirmation prompts, and `--color auto|always|never` controls colourisation.

## Exit codes

Scripts branch on these, so they are a stable contract. A bulk operation that partially fails never exits 0.

| Code | Symbol | Meaning |
|---|---|---|
| 0 | — | success |
| 1 | `DNS_ERROR` | generic or unexpected failure |
| 2 | `DNS_USAGE` | bad flags or arguments, including client-side rdata validation |
| 3 | `DNS_FORBIDDEN` | HTTP 403, or DNS not enabled for the project |
| 4 | `DNS_NOT_FOUND` | zone or record not found |
| 5 | `DNS_CONFLICT` | HTTP 409, or a record owned by another set |
| 6 | `DNS_INVALID` | HTTP 400 / 422 admission rejection |
| 8 | `DNS_UNAVAILABLE` | transport or connection failure |
| 9 | `DNS_ABORTED` | you declined a confirmation |

Errors render as an `Error:` line, an optional `Fix:` block, then the exit status with its symbolic name:

```console
$ datumctl dns zone describe nope.example
Error: zone "nope.example" not found in project acme-prod
Fix:   list the zones in this project:
       datumctl dns zone list
exit status 4   # DNS_NOT_FOUND
```

The underlying cause is shown only under `--verbose`, because for the common case it is Kubernetes plumbing you cannot act on.

## Troubleshooting

### "no project set"

```
Error: no project set
Fix:   pass --project, or set a default with:
       datumctl config set project <name>
```

The plugin has no default project and will not guess one. Either select a project for the session with `datumctl ctx use <org>/<project>`, or pass `--project <name>` on the command.

### DNS is not enabled for the project

```
Error: DNS is not enabled for project "acme-prod"
Fix:   enable it with:
       datumctl services enable dns.networking.miloapis.com --wait
exit status 3   # DNS_FORBIDDEN
```

Run that command. It is self-service and takes effect immediately. At a terminal the plugin offers to do it for you; in CI it refuses rather than hanging on a prompt, which is why you see this instead of a stalled job.

### Delegation is incomplete and the domain does not resolve

The zone exists and its records are programmed, but your **registrar** still points the domain somewhere else. Nothing in Datum can fix this — the change happens at whoever you bought the domain from.

Run `datumctl dns zone nameservers <domain>`, copy the nameservers it lists, and set them as the authoritative nameservers at your registrar. Then re-check with `--check`, which resolves the domain live rather than reporting what the control plane believes. Registrar changes propagate on the parent zone's TTL, so allow for a delay.

`Partial` means some but not all of the assigned nameservers are published — usually a half-finished edit at the registrar, and worth fixing even though the domain may appear to work.

### A record is stuck `Pending`

`Pending` means the DNS backend has not reported an outcome for that owner name yet. It is the normal state immediately after a write. If it persists, `datumctl dns record describe <domain> <name>` shows the full condition message from the server.

> [!NOTE]
> A freshly created record set has an entirely empty status — no conditions at all — until the controller writes one. The plugin renders that as `Pending`, and renders a never-transitioned timestamp as `—` rather than an age.

### `Conflict`

Another record already occupies that name in the DNS backend. The message is written by the backend and is shown verbatim, for example:

```
The record name is outside the zone. Check that the name belongs to this DNS zone.
```

The most common cause is a name that is not actually inside the zone — see the next entry. If the name is correct and the conflict persists with nothing in your project referencing it, the downstream copy may have been orphaned; see [DNS record conflict troubleshooting](../troubleshooting/dnsrecordset-downstream-orphan.md).

### `Not owner`

A different `DNSRecordSet` already owns that owner name, and the backend will not let two objects program the same name. The describe output names the competing object. Either edit the record through the set that owns it, or delete that set first.

This is also what you get when a platform-managed record is in the way. The operator creates a `<zone>-soa` and `<zone>-ns` record set for every zone, and the Gateway controller creates record sets labelled `dns.datumapis.com/source-kind: Gateway`. Gateway-owned records are read-only — editing them fights a controller that will revert the change.

### "already includes the zone domain"

```
Error: record name "www.example.com" already includes the zone domain
Fix:   names are relative to the zone — use "www", or "www.example.com." with a trailing dot to force an absolute name
```

This is the trap the plugin exists to close. The backend qualifies any name that does not end in a dot by appending the zone, so `www.example.com` in zone `example.com` becomes `www.example.com.example.com.`, which the backend then rejects as out-of-zone. Use the bare label `www`. The apex is `@`, not the domain name.

### A target is "not a fully qualified domain name"

```
Error: MX exchange "mail" is not a fully qualified domain name
Fix:   targets are absolute, not zone-relative — did you mean "mail.example.com."?
```

The mirror image of the previous problem. Owner names are relative; rdata targets are absolute. Add the trailing dot.

## See also

- [Architecture Overview](../architecture/README.md) — how the operator behind this API works
- [API Reference](../architecture/api-reference.md) — the `DNSZone` and `DNSRecordSet` schemas the plugin reads and writes
- [Troubleshooting](../troubleshooting/) — operator-side runbooks
