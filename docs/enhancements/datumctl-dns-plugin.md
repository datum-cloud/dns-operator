# `datumctl dns` plugin

| | |
|---|---|
| **Status** | Implemented |
| **Author** | Engineering |
| **Created** | 2026-08-22 |

> [!NOTE]
> This began as a design proposal and now describes shipped code. Everything here describes what the plugin does, unless it is marked **not built**. See [Phasing](#phasing) for what remains and [Open questions](#open-questions) for what is undecided.

## Summary

`datumctl-dns` is a first-party `datumctl` plugin. It lets developers manage DNS zones and records from the terminal without knowing that `DNSRecordSet` exists.

The plugin borrows its plumbing from the `compute` plugin and its ergonomics from `milo-ipam`. You enter records with named flags for the structured types and positionally for the flat ones. Zone-file presentation format works as input everywhere, so pasted values need no translation.

## Motivation

Outside the portal, the only way to manage Datum DNS is `datumctl apply -f` against raw `DNSRecordSet` YAML. That surface leaks the platform's internal shape three ways.

**The unit of storage is not the unit of thought.** One `DNSRecordSet` holds every record of one type for a whole zone, so `www`, `api`, and `@` share an object. Users think in records; the API stores type-buckets.

**The API accepts records that will never resolve, and that can delete records that do.** Nothing validates that the typed rdata field matches `spec.recordType`. The API admits a `recordType: A` entry carrying `cname: {...}`, and the backend then skips it. The skip leaves that owner name with an empty RRset, which turns the write into a *delete* of whatever was already there. The record never appears in DNS, may never get a status condition, and can take a working record with it.

**Failures hide one level down.** The top-level `Programmed` condition rolls up to a generic `Pending`. The interesting reasons — `NotOwner`, `Conflict`, `PDNSError` — exist only per owner name, in `status.recordSets[]`.

A CLI is the right place to fix all three, because all three are presentation problems over an API that is otherwise sound.

## Goals

- Present **records**, not record sets. The `(zone, type)` bucketing is an implementation detail that the CLI hides on read and reconstructs on write.
- Make the common path a single command with no YAML: add an A record, point a CNAME, set up MX.
- Validate client-side what the server does not, and refuse to submit a record that cannot resolve.
- Surface delegation state and per-record programming failures in words a user can act on.
- Stay a well-behaved sibling of `datumctl compute`: same plugin contract, output conventions, and flag vocabulary.

## Non-goals

- **DNSSEC.** No field, controller, or CRD property for it exists in the operator today.
- **Cross-zone or cross-project record search.** `DNSRecordSet.spec.dnsZoneRef` is a `LocalObjectReference`, so everything is namespace-local.
- **Replacing `datumctl apply -f`.** Power users keep the raw path; the plugin is the ergonomic one.

## Prior art

| Source | What the plugin takes from it |
|---|---|
| `datumctl compute` plugin | Plugin skeleton, `plugin.NewRootCmd`, entitlement pre-flight in `PersistentPreRunE`, table and footer and empty-state and `Next steps:` conventions, condition-to-human-word mapping, API-backed shell completion |
| `milo-ipam` plugin | Documented exit-code contract, `cliError` with a `Fix:` line, blast-radius-tiered confirmation, server-side `--dry-run` on every mutation, containment expressed as filter flags plus a client-side `tree` view |
| `cloud-portal` | The record domain model, TTL presets and `Auto`, `@` for apex, per-type validation tiers, BIND import and export, `DNSZoneDiscovery`-driven migration |
| Industry (BIND, `gcloud dns`, Cloudflare, `doctl`, octoDNS) | Zone-file presentation format for rdata, `name TTL IN TYPE rdata` ordering, declarative zone-file sync with a diff |

## Command surface

Nouns are singular with plural aliases, and verbs are explicit. The bare noun aliases to `list`, so muscle memory from `datumctl compute workloads` still works.

```
datumctl dns version [-o table|wide|json|yaml]

datumctl dns zone list [--status ok|pending|error] [--no-headers] [-o table|wide|json|yaml|name]
datumctl dns zone create <domain> [--description <text>] [--class <name>] [--no-wait] [--timeout <d>] [--dry-run]
datumctl dns zone describe <domain> [-o wide|json|yaml]
datumctl dns zone nameservers <domain> [--check] [--timeout <d>]
datumctl dns zone delete <domain> [--yes] [--dry-run]
datumctl dns zone import <domain> --file <zonefile> | --discover [--replace] [--dry-run] [--timeout <d>]
datumctl dns zone export <domain> [-f|--file <path>]

datumctl dns record list <domain> [--type A,MX] [--name www] [--status <word>] [--managed] [--no-headers] [-o ...]
datumctl dns record create <domain> <name> <TYPE> <rdata>... [--ttl <t>] [--wait] [--timeout <d>] [--force] [--dry-run]
datumctl dns record set    <domain> <name> <TYPE> <rdata>... [--ttl <t>] [--wait] [--timeout <d>] [--force] [--dry-run]
datumctl dns record delete <domain> <name> <TYPE> [<rdata>] [--yes] [--force] [--dry-run]
datumctl dns record describe <domain> <name> [<TYPE>]
datumctl dns record apply  <domain> -f <zonefile> [--prune] [--dry-run]
```

Aliases: `zones` and `z`, `records` and `rr`, `ls` for `list`, `show` and `get` for `describe`, `rm` for `delete`, `ns` for `nameservers`.

### Waiting

`--wait` is opt-in for `record create` and `record set`, and default-on for `zone create`. That is why the zone form spells the negative as `--no-wait`: a zone without nameservers cannot be delegated, so returning early would hand the user an unusable zone and no way to know it. `--timeout` bounds every wait and defaults to 2m.

`--force` permits editing a platform-managed record. See [Platform-managed records](#platform-managed-records).

### `version` runs offline

`datumctl dns version` prints the plugin version and the DNS API group-version. It uses no credentials, makes no API call, and needs no project or entitlement pre-flight.

You reach for a version check while debugging a broken login or an unreachable control plane. One that needs either is useless exactly when you want it.

### Why `create` and `set` are separate verbs

`create` appends a value to the RRset and fails on a duplicate. `set` replaces every value at that `(name, type)` with the ones given.

Route 53's `CREATE` and `UPSERT` draw the same distinction, as does octoDNS. It is the one users need: "add a second A record" and "change my A record" are different intents, and a single `create` cannot express both safely.

### Why the zone is a positional argument

Every record command takes the zone as its first argument. It is not optional and has no default. A mistyped zone that silently resolves to "the last one you used" is how people delete production records.

### Why there is no nested zone command

Following `milo-ipam`, containment lives in the data rather than the command path. The hierarchy appears as a positional argument, and as a rendered view in `zone describe`.

## Record grammar

The plugin accepts two notations. Flat types are positional, because nothing needs disambiguating. Structured types are taught with named flags, because positional numerics are unreadable. Zone-file presentation format is always accepted as input, because that is what people paste from a provider export, a `dig` output, or an "add this TXT record" page.

### Flat types take positional values

Repeat the argument to make a multi-value RRset.

```sh
datumctl dns record create example.com www A 203.0.113.10
datumctl dns record create example.com www A 203.0.113.10 203.0.113.11 --ttl 300
datumctl dns record set    example.com @   TXT "v=spf1 include:_spf.example.com ~all"
datumctl dns record create example.com cdn CNAME lb.example.net.
```

### Structured types take named flags

These are the types where `"10 5 5060 sipserver.example.com."` is opaque to whoever reads it back six months later.

```sh
datumctl dns record create example.com @ MX --preference 10 --exchange mail.example.com.
datumctl dns record create example.com _sip._tcp SRV \
  --priority 10 --weight 5 --port 5060 --target sipserver.example.com.
datumctl dns record create example.com @ CAA --flag 0 --tag issue --value letsencrypt.org
datumctl dns record create example.com api HTTPS --priority 1 --target . --param alpn=h3,h2 --param port=443
```

### Presentation format also parses

Every type accepts it, so a pasted value works without translation.

```sh
datumctl dns record create example.com _sip._tcp SRV "10 5 5060 sipserver.example.com."
datumctl dns record create example.com @ CAA '0 issue "letsencrypt.org"'
```

| Type | Presentation grammar | Named flags | Parsed into |
|---|---|---|---|
| A / AAAA | `<ip>` | positional only | `a.content` / `aaaa.content` |
| CNAME / ALIAS / NS / PTR | `<hostname>` | positional only | `.content` |
| TXT | `<string>` | `--data` (the command layer expands `@file` and `-`) | `txt.content` |
| MX | `<preference> <exchange>` | `--preference --exchange` | `mx.{preference,exchange}` |
| SRV | `<priority> <weight> <port> <target>` | `--priority --weight --port --target` | `srv.{priority,weight,port,target}` |
| CAA | `<flag> <tag> <value>` | `--flag --tag --value` | `caa.{flag,tag,value}` |
| TLSA | `<usage> <selector> <matchingType> <certData>` | `--usage --selector --matching-type --cert-data` | `tlsa.{...}` |
| HTTPS / SVCB | `<priority> <target> [k=v ...]` | `--priority --target --param k=v` | `https.{priority,target,params}` |

Mixing positional rdata and named flags for the same value is a usage error, not a merge.

### Whole-line paste

`--line` takes a `dig`-shaped line and parses the name, TTL, type, and rdata out of it. The paste case therefore never distorts the main grammar.

```sh
datumctl dns record create example.com --line "www 300 IN A 203.0.113.10"
```

The line's TTL is used, so that record is written with a TTL of 300 rather than `Auto`. The class (`IN`) and the TTL are both optional and may appear in either order, so `www A 203.0.113.10` and `www IN 300 A 203.0.113.10` also parse. An explicit `--ttl` overrides whatever the line carries, because the flag is the more specific instruction. Supplying rdata positionally or by named flag alongside `--line` is a usage error, on the same rule as mixing the other two notations.

### Each notation teaches the other

A mutation driven by flags confirms in presentation format. `describe` on a record entered as presentation format shows the named fields. Neither grammar stays undiscovered.

```
$ datumctl dns record create example.com @ MX --preference 10 --exchange mail.example.com.
  record/example.com MX @ created
  @  Auto  IN  MX  10 mail.example.com.
```

### TXT quoting

Shell quoting bites hardest on SPF and DKIM values, so TXT also accepts `--data @path/to/file` and stdin as `--data -`. That expansion happens in the command layer through `rdata.ResolveTXTData`, not inside flag parsing: `FromFlags` stays a pure function over the flag set and returns `--data` verbatim, which keeps the file and stdin reads injectable and testable. Values over 255 characters are chunked into multiple quoted strings on write, per RFC 1035.

### TXT has two representations

This is the sharpest edge in the record grammar.

In memory, `txt.content` always holds the **logical** value — the string the user typed, `v=DMARC1; p=none`. The API must receive the **wire** form: quoted, escaped, and split into 255-byte character-strings.

`quoteIfNeeded` wraps `txt.content` in a *single* quoted string unless it is already quoted end to end. A value submitted logically is therefore corrupted five ways:

- Over 255 bytes, it exceeds the character-string limit.
- An embedded quote is left bare and terminates the string early.
- A trailing backslash escapes the closing quote and runs the corruption past the end of the value.
- An embedded backslash reads back as an escape.
- A control character is emitted literally, producing a zone file that cannot be parsed back at all.

> [!WARNING]
> Write paths must call `rdata.EntryForAPI(type, entry)` immediately before submitting, for every type.

`EntryForAPI` is idempotent and does not mutate its argument. Today it rewrites only TXT, but a write path that says "encode this entry" keeps working if another type grows a wire form, where one that special-cases TXT does not. Read paths need nothing: `Key`, `Equal`, `Render`, `Fields`, and `Validate` all decode defensively, so an entry straight off the API behaves correctly whether or not the caller decoded it. `rdata.EntryFromAPI` exists for a caller that wants to hold the logical value itself.

This is enforced in code rather than in prose for one reason. The semicolon case — the one anybody checks by hand — behaves **identically** with and without the helper, because `quoteIfNeeded` escapes semicolons itself. A caller who forgets the helper therefore passes every obvious manual test and ships the other four landmines undetected.

### Why validation runs on every write path

The CLI validates the type and field pairing, the rdata arity, and the per-field syntax before submitting. The API server does not: there is no validating webhook and no CEL rule tying the typed field to `spec.recordType`. The consequence is worse than a record that fails to appear.

`buildRRSets` registers an owner's RRset through `getOrInit` (`internal/pdns/client.go:432`) *before* checking whether the entry's typed field is nil. A mismatched entry therefore leaves an RRset with **zero records**, rather than no RRset at all. `BuildOwnerRRSet` returns `ok=true` with an empty `Records`, and the reconciler guards on `if ok && len(payload.Records) > 0` rather than on `ok` alone (`internal/controller/dnsrecordset_powerdns_controller.go:142`). So `wantDelete`, set true at line 138, is never cleared, and line 157 calls `DeleteRRSet`. That sends an explicit `changetype: "DELETE"`.

The deletion is not PowerDNS inferring intent from an empty RRset. This codebase asks for it. The pdns client author knew an empty REPLACE was unviable: `ApplyRecordSetAuthoritative` carries the comment "If an rrset has 0 records, PDNS will reject a REPLACE. Convert it to a DELETE instead."

A single wrong typed field therefore **removes the existing, correct RRset at that name**, with no error condition anywhere to say so.

Two bounds, because the accurate claim survives scrutiny better than the alarming one:

- The delete is per owner name, and needs *every* entry at that owner to yield nothing. An owner with one good entry and one mismatched entry gets a `REPLACE` that silently drops the bad value, which is the phantom outcome rather than the delete. This still matters enormously, because the single-entry record is the common case.
- Exactly one guard exists, and it does not help here. `aliasedOwnerExists` suppresses the delete when a different spelling of the same owner still claims the RRset — `api` versus `api.example.com.` versus `@`. It protects owner-name rewrites, not wrong-typed-field entries.

The requirement is not "validate on the create path". Validation runs on **every** write path: `record create`, `record set`, `record apply`, `zone import`, and any future bulk or scripted mutation. A malformed entry arriving through an imported zone file deletes production records exactly as surely as one typed by hand. `rdata.Validate` covers a single entry, and `rdata.ValidateEntries` covers the constraints that exist only across a whole record set. Write paths call both.

### Names, apex, and the trailing dot

The operator qualifies owner names backend-side. A name **ending in a dot is absolute**; anything else is suffixed with the zone. So `www.example.com` without a dot, in zone `example.com`, becomes `www.example.com.example.com.`, which PowerDNS rejects as out-of-zone.

The CLI closes that trap:

- Names are always zone-relative. `@` means apex, and `www`, `*`, `_dmarc`, and `_sip._tcp` are labels.
- A name that already ends in the zone domain is rejected with a fix, not silently qualified:

  ```
  Error: record name "www.example.com" already includes the zone domain
  Fix:   names are relative to the zone — use "www", or "www.example.com." with a
         trailing dot to force an absolute name.
  ```

- A trailing dot is honoured as an explicit absolute name. The CLI warns if the result falls outside the zone.
- Input is lowercased before submission, because `spec.domainName` is lowercase-only at admission.

Target fields inside rdata — MX exchange, SRV and HTTPS target, CNAME and ALIAS and NS and PTR content — are absolutized backend-side by appending a dot. A relative `mail` becomes `mail.`, a root-relative name, which is almost certainly not what was meant. The CLI therefore requires an FQDN with a trailing dot in every target field, and offers the fix inline:

```
Error: MX exchange "mail" is not a fully qualified domain name
Fix:   targets are absolute, not zone-relative — did you mean "mail.example.com."?
```

### TTL

`--ttl` takes seconds, or a duration built from `s`, `m`, `h`, `d`, and `w`: `--ttl 300`, `--ttl 5m`, `--ttl 1h`, `--ttl 1d`, `--ttl 1h30m`. Omitting it means `Auto`, the backend default of 300s, which `describe` renders as `Auto (5m)` so the number is never a mystery.

Displayed TTLs always carry their unit and use the largest one that divides evenly, so `300` shows as `5m` and `86400` as `1d`. A bare number in a TTL column cannot be read without guessing at its unit. Every spelling shown parses back to the same number.

Unlike the portal, the CLI does not snap arbitrary TTLs onto a preset ladder: `240` stays `240`. The portal's rounding to 300 exists to serve a dropdown, and silently rewriting an imported TTL is the wrong default for a tool people script.

#### One TTL wins per owner name

TTL is per-RRset in DNS but per-entry in the API, and the two cannot both be right. `buildRRSets` creates the RRset for an owner name from the **first** entry it sees and takes the TTL from that entry. Every later entry for the same owner contributes its value, but its TTL is discarded.

A record set whose entries disagree on TTL is not an error at admission, not an error in the backend, and not visible in any condition. One TTL wins and the others evaporate.

The CLI closes this on both sides:

- **On write**, it applies `--ttl` to every value it writes for that `(name, type)`, so a set it produces can never disagree with itself.
- **On read**, it compares the entries for each owner and warns when they differ, naming the value that took effect. `rdata.Warnings` emits `values for "www" disagree on TTL (300 and 60) — the DNS backend applies the first one, 300, to the whole record set` when passed more than one entry.

This matters most for imported and hand-edited sets, which are the ones the CLI did not write.

#### Compare TTLs by effective value, never by pointer

`util.TTLEqual` treats a nil TTL — `Auto` — as equal to an explicit `util.DefaultTTL` of 300, because those resolve to the same thing in DNS.

That is what keeps the export-to-apply loop closed. A zone file cannot spell `Auto`: `zone export` writes a `$TTL` directive and omits the per-record TTL, and re-reading resolves that to an explicit 300. A pointer comparison then reports every `Auto` record as changed, so `record apply` rewrites the whole zone on every run and a drift check built on `--dry-run` alarms forever.

That is not hypothetical. It shipped, and the round-trip test that caught it lives in the plugin's end-to-end harness. Read that test before simplifying this back to comparing pointers.

## CLI-side validation policy

Everything in this section is **CLI policy, not platform enforcement**. The API server admits every record that breaks these rules.

They are written down because a rule that lives only in a validator is a rule nobody can review. The distinction between "the platform rejects this" and "the CLI declines to send this" is the difference between a constraint and an opinion, and the second kind needs a justification.

The dividing line: a `+kubebuilder` marker in `api/v1alpha1/dnsrecordset_types.go` is enforced at admission, and the CLI mirrors it only for a better error message. Everything below has no marker behind it.

### Fields with no validation at all

`TXTRecordSpec.Content`, `PTRRecordSpec.Content`, every field of `TLSARecordSpec`, `HTTPSRecordSpec.Target` and `.Params`, and `RecordEntry.TTL` carry no kubebuilder validation whatsoever. The CLI supplies it.

**TTL is bounded to 0–2147483647.** RFC 2181 §8 defines the TTL as a 31-bit unsigned quantity. The field is a bare `*int64` with no bounds, so a negative TTL, or one past 2³¹, is admitted today and handed to PowerDNS as an `int`. TTL `0` is legal DNS and is accepted, but it warns: most people who type `0` mean "let the platform choose", which is `--ttl auto` and a nil TTL, rather than "instruct every resolver on the internet never to cache this".

**TXT data must be non-empty and at most 2048 bytes**, measured on the logical value. The 2048 limit is the portal's per-record cap and the figure most providers publish, borrowed rather than invented so the two clients agree. Measuring the wire form would reject a record the CLI itself created, because escaping and chunking inflate the byte count: a legal 2040-byte value encodes to 2063 and becomes uneditable.

**Control characters are permitted, and warned about.** They were rejected at first, on the grounds that presentation format could not carry them. That stopped being true once the renderer learned the RFC 1035 §5.1 `\DDD` decimal form. Keeping the rule would have broken the export-and-apply loop for any record created elsewhere: the CLI would export it correctly and then refuse to read its own file back. A newline in a TXT value is usually a paste accident rather than an intention, which is what a warning is for.

**TLSA gets the full RFC 6698 range check**: usage 0–3, selector 0–1, matching type 0–2, and certificate data as hexadecimal with an even digit count. Beyond the ranges, matching type 1 must carry exactly 64 hex digits and matching type 2 exactly 128. That is stricter than a range check, deliberately. A SHA-256 digest is always 32 bytes and a SHA-512 always 64, so a digest of any other length is not an unusual choice but a **broken record** — and a broken TLSA record breaks TLS for the name it covers, rather than merely failing to resolve. No legitimate input is rejected by this rule.

**HTTPS and SVCB** targets must be non-empty, and either a bare `.` or a fully qualified host name. Parameter values may not contain whitespace, because a space splits the value into a second parameter when the line is serialized. A non-flag key with an empty value is rejected rather than dropped, since `encodeSvcbParams` skips it silently. Alias mode — priority 0 — may not carry parameters and may not target `.`: `encodeSvcbLine` emits only `"<priority> <target>"` when the priority is zero, so parameters set alongside it are discarded on the way to the zone. The CLI refuses rather than letting the user believe they were written.

**PTR content** must be a valid host name with a trailing dot, the same rule the other target fields get.

### Rules layered on fields that do have markers

**Owner-name structure.** The CRD pattern `^(@|[A-Za-z0-9*._-]+)$` constrains the character set but not the shape, so `a..b`, a 64-character label, `*www`, and `dev.*` are all admitted today. The CLI rejects empty labels, labels over 63 characters, names over 253 characters, partial wildcard labels, and wildcards that are not the leftmost label.

**A CNAME may not exist at the zone apex**, and the fix points at `ALIAS`. RFC 1034 forbids a CNAME coexisting with the SOA and NS records that must exist at the apex. PowerDNS agrees, but only after admission, as a `Conflict` condition the user has to go find. **An SOA may not exist anywhere but the apex**, for the mirror-image reason.

**SOA `rname` must be a mailbox in dot notation.** A literal `@` is rejected with a fix explaining the notation, where `admin@example.com` becomes `admin.example.com.`. The value must have at least three labels, so a bare domain does not pass as an address. This follows portal precedent.

**A null MX is `0 .` or nothing.** An exchange of `.` at preference 0 is the RFC 7505 declaration that the domain accepts no mail. The same `.` at any other preference is a typo rather than a policy, and is rejected. An SRV target of `.` is accepted at any priority, because RFC 2782 gives it the distinct meaning "this service is decidedly not available here". An NS content of `.` is rejected outright.

**Host-name strictness is tiered**, following the portal:

| Tier | Applies to |
|---|---|
| Strict RFC 1123, no underscores | MX exchange, NS content, SOA mname |
| Permissive, underscores anywhere in a label | CNAME, ALIAS, PTR content, SRV target |
| Permissive plus a bare `.` | SVCB and HTTPS targets |

This tier list is CLI-side because the CRD's own patterns disagree with each other in ways that look accidental rather than designed. `NSRecordSpec` has a no-underscore pattern. `CNAMERecordSpec` and `ALIASRecordSpec` have underscore-permitting patterns. `MXRecordSpec.Exchange`, `SRVRecordSpec.Target`, `PTRRecordSpec.Content`, and `SOARecordSpec.MName` have nothing beyond `MinLength=1`. Rather than inherit that inconsistency, the CLI applies one coherent scheme and documents it here. Underscored service names such as `_domainconnect.gd.domaincontrol.com.` are common enough that forbidding them everywhere would be wrong, and allowing them in an MX exchange would be equally wrong.

### CAA tags: accept the API's range, warn on the unfamiliar

The API pattern is `^[a-z0-9]+$`. The portal narrows it to an enum of `issue`, `issuewild`, and `iodef`. The CLI takes the API's range and warns on tags outside the known set, rather than copying the portal.

There are two reasons.

RFC 8657 added `contactemail` and `contactphone` after the portal's schema was written, and `issuemail` is in the IANA registry as well. The portal's three-value enum is out of date, and inheriting it would make the CLI reject records that the standard defines and the server accepts.

More generally, **a client stricter than the server it talks to is a client bug**. A user with a legitimate tag would have no way to create the record through the plugin, and would fall back to `datumctl apply -f` — precisely the fallback this plugin exists to remove.

Malformed tags remain hard errors: uppercase, hyphens, anything outside `[a-z0-9]+`. Those violate the CRD pattern and would be rejected at admission anyway. The warning set is `issue`, `issuewild`, `iodef`, `issuemail`, `contactemail`, and `contactphone`. The CAA flag gets the same treatment: any value 0–255 is accepted, matching the marker, and anything other than 0 (non-critical) or 128 (critical) warns.

### When to warn instead of error

The general rule: **error when the platform will discard or corrupt the input, and warn when the input is merely unusual.**

Errors are for a record that cannot work: a mismatched typed field, a relative target, a multi-value CNAME, a wrong-length TLSA digest.

Warnings are for a record that will work and might not be what was meant: an unfamiliar CAA tag, an unusual CAA flag, an unregistered SVCB parameter key, SOA timers below the RFC-recommended minimums, SRV port 0, TTL 0, a control character in TXT data, and an owner name whose entries disagree on TTL.

A user who has a reason for the unusual thing should never have to argue with their tools about it.

## Output

### `zone list`

```
NAME              STATUS    RECORDS   NAMESERVERS                      DELEGATED       AGE
example.com       OK        12        ns1.datum.net., ns2.datum.net.   yes             14d
partial.acme.io   OK        6         ns1.datum.net., ns2.datum.net.   partial (1/2)   9d
old.acme.io       OK        8         ns1.datum.net., ns2.datum.net.   no              21d
staging.acme.io   Pending   2         —                                unknown         3m

4 zones — 3 OK, 1 Pending, 0 Error
```

`DELEGATED` carries the four delegation states rather than a boolean: `yes`, `no`, `partial (N/M)`, and `unknown`. A zone with no nameservers assigned yet reads `unknown` rather than `no`. See [Unknown is not a weaker Incomplete](#unknown-is-not-a-weaker-incomplete) for why.

`-o wide` appends `CLASS` and `DOMAIN`, the linked `Domain` object. The footer tally is computed after filtering, matching compute: `--status pending` reports the count of what it printed, not of the project.

### `record list`

Rows are flattened from every `DNSRecordSet` in the zone, sorted by name, then type, then value.

```
NAME       TYPE   TTL   VALUE                          STATUS
@          SOA    1h    ns1.datum.net. hostmaster…     Programmed  (platform)
@          NS     1h    ns1.datum.net.                 Programmed  (platform)
@          NS     1h    ns2.datum.net.                 Programmed  (platform)
@          MX     5m    10 mail.example.com.           Programmed
www        A      5m    203.0.113.10                   Programmed
www        A      5m    203.0.113.11                   Programmed
api        CNAME  Auto  lb.example.net.                Conflict
_dmarc     TXT    1h    "v=DMARC1\; p=none"            Programmed

8 records — 6 Programmed, 1 Conflict, 1 Pending
```

The `STATUS` word comes from the per-owner-name condition in `status.recordSets[]`, never from the rolled-up top-level one. As in compute, the full sentence with the server's message appears in `describe`.

The semicolon in the `_dmarc` row is escaped because a semicolon starts a comment in zone-file presentation format. The stored value is the logical `v=DMARC1; p=none`; only the rendering escapes it, in `record list` and in an exported zone file alike. See [TXT has two representations](#txt-has-two-representations) for why the two forms are kept apart.

One owner name can appear in `status.recordSets[]` more than once, because a single bucket may hold two spellings of the same name: `www` and `www.example.com.` are one RRset to the backend. The status shown is the **worst** across every matching entry. A record is therefore never reported live while another spelling of it is in `Conflict`, and an unrecognised reason outranks both `Pending` and `Programmed` rather than being mistaken for success.

### Status filter tokens

`--status` accepts the first word of a status, or the full status with spaces folded to hyphens: `--status conflict`, `--status not-owner`.

Compute's plain "first word" rule does not survive contact with this vocabulary. The first word of `Not owner` is `not`, which is no kind of filter. The shipped command accepts `not-owner`, `notowner`, `not`, and the folded full string, so every spelling anyone would reach for works. Teach `not-owner`.

> [!IMPORTANT]
> The set of status words is open at the bottom, so the filter's accepted tokens cannot be a closed list.

An unrecognised server reason passes through raw as the status word, deliberately, rather than being flattened to a generic failure. A row can therefore legitimately read `Throttled` for a reason this code has never heard of. A validator that rejects every token outside a fixed set would refuse `--status throttled` with exit 2 while the matching row sits visibly in the table: the tool would render a value and deny it exists.

The two properties have to be designed together. Passing unknown reasons through is what keeps the CLI honest about a server that grows new failure modes, and a closed filter list is what quietly takes that back. The documented tokens are the vocabulary worth teaching, not the limit of what is accepted.

`unknown` is deliberately not an accepted token. `util.StatusUnknown` is returned only for a nil record set, which is unreachable for a row that exists, so accepting it would advertise a filter that can never match anything.

### Status vocabulary

| Condition | Rendered | Describe detail |
|---|---|---|
| `Programmed=True` | `Programmed` | — |
| `Programmed=False/NotOwner` | `Not owner` | `Another record set owns this name — <competing object>` |
| `Programmed=False/Conflict` | `Conflict` | the backend's `FriendlyMessage`, verbatim |
| `Programmed=False/PDNSError` | `Error` | the backend's `FriendlyMessage`, verbatim |
| `Programmed=False/Pending`, or no per-name status | `Pending` | `waiting for the DNS backend` |
| `Accepted=False` on the set | `Rejected` | the `Accepted` message, verbatim |

The backend's `FriendlyMessage` strings are already written for humans: "The record name is outside the zone. Check that the name belongs to this DNS zone." Show them as-is. Following the rule in compute's `conditions.go`, the CLI prettifies the handful of known reasons and passes every other reason through raw rather than guessing.

> [!NOTE]
> A freshly created `DNSRecordSet` has an entirely empty `status`, so the CLI renders "no status yet" rather than assuming the conditions array exists.

An earlier draft of this document said the object arrived carrying CRD-defaulted conditions stamped `lastTransitionTime: 1970-01-01T00:00:00Z`. That default was on the Go type but never reached the served CRD, because controller-gen drops defaults on a status that has a status subresource. Regenerating produced a byte-identical CRD, which is what proves it was dead rather than merely unused. Seeded conditions, if anyone wants them, have to come from the mutating webhook or a controller write.

### `zone describe`

```
Zone         example.com                     project: acme-prod
Class        datum-external-global-dns
Created      14d ago

Status       OK — zone programmed, 12 records live
Delegation   Complete — all 2 nameservers set at the registrar

Nameservers
  ns1.datum.net.                              set at registrar
  ns2.datum.net.                              set at registrar

Records      12 across 6 types
  SOA 1    NS 2    A 3    CNAME 1    MX 1    TXT 4

Next steps:
  List records:            datumctl dns record list example.com
  Add a record:            datumctl dns record create example.com www A 203.0.113.10
  Export as a zone file:   datumctl dns zone export example.com
```

The per-type breakdown follows the portal's type ordering rather than alphabetical or count order, so the two clients read the same way.

The summary reconciles two independently maintained numbers: the per-type counts derived from the zone's record sets, and `status.recordCount` on the zone itself. When they disagree it says so inline, rather than printing figures that do not add up. When no record sets come back at all, it explains why there is no breakdown instead of printing a bare number.

```
Records      12 across 4 types
  A 3    CNAME 1    MX 1    TXT 4
  the per-type counts add up to 9, not the 12 the zone reports — the operator is still catching up
```

When delegation is genuinely incomplete — the registrar has been checked and points elsewhere — the block becomes the instruction:

```
Delegation   Incomplete — 0 of 2 nameservers set at the registrar

Set these nameservers at your domain registrar:
  ns1.datum.net.
  ns2.datum.net.

Currently delegated to:
  ns-cloud-a1.googledomains.com.
  ns-cloud-a2.googledomains.com.

Re-check with: datumctl dns zone nameservers example.com --check
```

### How delegation state is computed

The CLI computes delegation state client-side, comparing `status.nameservers` — the nameservers Datum assigned — against `status.domainRef.status.nameservers[].hostname`, the ones observed at the registrar. Both sides are normalized: lowercased, trailing dots stripped, surrounding whitespace trimmed.

Verification lives on the `Domain` object rather than the `DNSZone`, so `zone describe` reads both. The returned lists keep the API's own spelling, trailing dots included, because those are the strings the user has to paste into a registrar.

There are four states, not two:

| State | Meaning |
|---|---|
| `Complete` | The registrar publishes every assigned nameserver. |
| `Partial` | The registrar publishes some but not all of them, usually a half-finished edit. |
| `Incomplete` | The registrar **was checked** and publishes none of them. |
| `Unknown` | There is nothing to compare: no nameservers assigned yet, no linked `Domain`, or a linked `Domain` whose nameservers have not been observed. |

### Unknown is not a weaker Incomplete

The difference is a correctness rule, not a presentation choice. `Incomplete` means the registrar was looked at and points somewhere else. `Unknown` means nobody has looked. An empty observed list is the second, and it is the ordinary state for the first minutes after a zone is created, before the `Domain` controller has resolved anything.

> [!IMPORTANT]
> The CLI must never state anything about a registrar it has not observed.

Concretely, in every `Unknown` case:

- The "Set these nameservers at your domain registrar" block is withheld.
- No "Currently delegated to" list is printed.
- Where there are assigned nameservers to annotate, each is marked `unknown` rather than `not set at registrar`.
- Where there are none, the list reads `none assigned yet`.

The instruction block appears for `Incomplete` and `Partial` only, the two states where the user has something to do.

This shipped as a bug and was fixed. `DelegationState` guarded on whether a `Domain` was linked, rather than on whether anything had been observed, so a freshly created zone reported `Incomplete (0 of 2)` and told the user in writing that their registrar was misconfigured. It is written down as a rule because a future refactor that treats "no data" as "no delegation" would reintroduce it, and the output would look entirely plausible.

`Delegation.Linked` distinguishes the two `Unknown` cases that are not about the nameserver list — no `Domain` at all, versus a `Domain` nobody has checked — so the summary line can say which. `zone describe` and `zone nameservers` share a single `delegationNeedsAction` predicate rather than repeating the condition, because a compound condition duplicated across two call sites is how a third caller gets it wrong.

`--check` additionally resolves the zone's NS records live against the assigned nameservers, which answers "is it actually working" rather than "is the control plane happy".

## Errors and exit codes

The plugin adopts `milo-ipam`'s contract wholesale. It is the only one of the two reference plugins that has one, and scripts need it.

| Code | Symbol | Trigger |
|---|---|---|
| 0 | — | success |
| 1 | `DNS_ERROR` | generic or unexpected |
| 2 | `DNS_USAGE` | bad flags or arguments, including client-side rdata validation |
| 3 | `DNS_FORBIDDEN` | HTTP 403, HTTP 401, or DNS not entitled for the project |
| 4 | `DNS_NOT_FOUND` | zone or record not found |
| 5 | `DNS_CONFLICT` | HTTP 409, or a record owned by another set |
| 6 | `DNS_INVALID` | HTTP 400 or 422, admission rejection |
| 8 | `DNS_UNAVAILABLE` | transport failure, HTTP 429, or any HTTP 5xx |
| 9 | `DNS_ABORTED` | user declined a confirmation, or the command was interrupted |

> [!IMPORTANT]
> Exit 8 is the retryable one, and it covers server-side failures as well as network ones.

A 429 and every 5xx map to `DNS_UNAVAILABLE`, alongside a refused dial or a timeout, because none of them produced a verdict. The request may or may not have been seen, and trying again is the correct response to all of them. Automation should retry on 8 and on nothing else.

An earlier version of this table listed 8 as "transport failure" only. A script written against it would retry a refused connection but give up on a server outage — the case where retrying matters most.

Three classifications are worth stating, because the HTTP code alone does not imply them:

- **401 is exit 3, not exit 1.** An expired session is the most common failure a real user hits, and it gets the fix line that names `datumctl login`.
- **Interruption is exit 9, not exit 1.** Ctrl-C cancels the in-flight request through a signal-aware context and reports as `DNS_ABORTED`, the same code as declining a confirmation. In both cases the user chose not to proceed.
- **Transport failures are detected by error type, never by matching the message text.** `net.Error`, `*net.OpError`, `*net.DNSError`, `*url.Error`, the `tls` verification errors, `context.DeadlineExceeded`, and `io.ErrUnexpectedEOF` are transport; everything else is not. A substring matcher was tried first, and classified any message containing `tls` or `eof` as a network failure — so a client-side "invalid TLSA digest" told the user to check their internet connection. Matching a rendered message is guessing at a value the sender never promised.

Errors render as `Error:`, then an optional `Fix:` block, then `exit status N   # DNS_CONFLICT`. The underlying cause appears only under `--verbose`. Messages are lowercase with no trailing period, identifiers are quoted with `%q`, and wrapped infrastructure failures take gerund prefixes such as `listing record sets: %w`. This matches compute.

`UserError` is not importable from a plugin, because it lives in `datumctl/internal/errors`. This is a local `cliError` type, as in ipam.

## Mutation safety

### Read-modify-write with a precondition

Every record mutation fetches the `(zone, type)` set through the server-side field selectors the CRD already declares — `spec.dnsZoneRef.name` and `spec.recordType` — edits `spec.records`, and patches back with `resourceVersion` set.

The portal omits the precondition, so two concurrent writers to the same type silently clobber each other. The CLI sends it, retries once on conflict, then reports:

```
Error: the A records for example.com changed while this command was running
Fix:   re-run the command — someone else modified the same record type.
```

### Confirmation tiers

Confirmation scales to blast radius, as in ipam.

| Action | Gate |
|---|---|
| `record create` / `set` | none |
| `record delete` | `y/N` prompt; proceeds without prompting when non-interactive |
| `record apply` (adds and updates only) | `y/N` prompt on the diff; proceeds when non-interactive |
| `record apply --prune` with deletions | `y/N` prompt; **refuses** non-interactively without `--yes` |
| `zone delete` | type the zone name to confirm; **refuses** non-interactively without `--yes` |

The two tiers differ in what happens when nobody can answer. A recoverable action proceeds, because a prompt that cannot be answered should not block a script. An unrecoverable one refuses, because nothing brings back a deleted RRset or a deleted zone.

Prompts go to stderr, so they never pollute `-o json` on stdout. A single command that prompts twice — the entitlement pre-flight, then a confirmation — must see both answers, which is why the readers do not buffer ahead.

`zone delete` states the cascade explicitly. The operator sets a controller `ownerReference` from the zone onto every record set, so deleting a zone garbage-collects all of its records.

```
Deleting zone example.com will also delete all 12 DNS records it contains.
This cannot be undone, and the domain will stop resolving.

Type the zone name to confirm: _
```

### `--dry-run` on every mutation

Dry run uses the API server's server-side dry-run, so validation and admission actually run. The output is the diff that would be applied.

The preview is the same code path as the write, not a separate rendering of it. The `--replace` incident described in [Compare owner names qualified](#compare-owner-names-qualified) verified this: `--dry-run` printed diff lines and counts identical to the real run. That is the property worth having. The preview told the truth about what the command would do, even while the decision behind it was wrong. A dry run that agreed with a correct write but diverged from a buggy one would be worse than none.

### `--wait`

`--wait` polls until the affected owner names report `Programmed=True`, with a bounded timeout, printing the per-name outcome.

DNS programming is asynchronous, and a command that returns before the record resolves invites "the CLI lied to me". Compute solves the same problem by always tailing the rollout. Here the write is small enough that waiting is opt-in for `create` and `set`, and default-on for `zone create`, which must wait for nameserver assignment to be useful at all.

## Platform-managed records

The operator creates a `<zone>-soa` and a `<zone>-ns` record set for every zone. The Gateway controller creates record sets labelled `dns.datumapis.com/source-kind: Gateway`. None of the operator-created ones carry a marker label — the guard is existence-based, so a user who deletes them gets them recreated.

The CLI marks both categories in `record list`, as `(platform)` and `(managed by AI Edge)`, and treats them differently:

- **Gateway-owned records are read-only.** Editing them fights a controller that will revert the change.
- **SOA and apex-NS records are warned, not blocked.** Editing is permitted with `--force`, because the API allows it and the operator never reconciles their content. The warning names the risk: `editing apex NS records can break delegation`.

Identifying the operator-created pair is necessarily heuristic: type `SOA`, or an NS record at the apex, tested by qualified name rather than by the literal string `@`. Adding a real provenance label to the operator would let the CLI stop guessing. That is the one [open question](#open-questions) with a correctness consequence rather than an ergonomic one.

### Ownership by fact, not by inference

The plugin decides who owns a record three ways, and only one of them is a fact.

The platform tier is an inference from shape: a record is the platform's if it is the zone's SOA, or an NS record at the apex. The operator stamps nothing. It creates `<zone>-soa` and `<zone>-ns` and relies on their existence, so the CLI has no choice but to guess — and every guess it has made has eventually been wrong in some spelling or some object name.

The Gateway tier is different. The Gateway controller labels what it creates, so the CLI reads a fact the producer wrote rather than drawing a conclusion from a name.

That difference has a consequence worth stating plainly. **The Gateway tier is structurally immune to the owner-name class of bug, because it never compares an owner name at all.** Four separate guards in this plugin were built on a literal apex test, and all four failed open on a record stored as `example.com.` rather than `@` — a spelling the API permits and `pdns.QualifyOwner` resolves to the apex. None of them could have touched the Gateway tier, because there is no name in it to spell two ways. It is also the only ownership mechanism in the plugin that never appeared in a review finding.

The test ORs three labels — `dns.datumapis.com/source-kind`, `dns.datumapis.com/managed`, and `app.kubernetes.io/managed-by` — rather than keying on source-kind alone. Source-kind does not appear to be the label the producer treats as load-bearing: its own garbage collector selects on managed-by, managed, source-name, and source-namespace, and pointedly not on source-kind.

That producer lives in **network-services-operator**, in `internal/controller/gateway_dns_controller.go`, in `garbageCollectDNSRecordSets`. No Gateway controller exists anywhere in this repository, so a reader grepping locally for these labels finds only the CLI's own constants and may conclude they are invented. They are not.

The constants also cannot be imported, whatever version is pinned. They live under the producer's `internal/` tree, and Go's internal-package rule forbids importing them from outside that module. The duplicated string literals here are forced rather than chosen. This is the one duplicated policy in the whole audit that cannot be collapsed onto a shared definition — every other instance was fixable by consolidation, and this one can only be watched. That is another reason to keep the CLI's copy in exactly one place.

> [!WARNING]
> Grepping the dependency does not settle it either. This module pins network-services-operator v0.9.0, which does not contain that file at all.

The file first appears around v0.17.0, and the label strings occur nowhere in the pinned version. The labels can only be checked against the producer's own repository, not against anything in `go.mod`. That is also why the citation names functions rather than line numbers: nothing here pins them, so they will drift.

## Bulk paths

Two bulk paths, both with backing in the platform.

### `zone import --file <zonefile>`

Import parses BIND format, rewrites an apex `CNAME` to `ALIAS` and flags the rewrite, reports unsupported types rather than dropping them silently, and groups by type so each `(zone, type)` set is written once. Types the operator does not support are listed, not swallowed.

Import also refuses the zone's apex NS records and its SOA, reports each as skipped with a reason, and imports everything else.

```
NAME   TYPE   TTL    VALUE                                              RESULT
www    A      5m     203.0.113.10                                       created
@      NS     1h     ns1.oldprovider.net.                               skipped — the zone's apex NS records are managed by the platform — importing them would break delegation
@      NS     1h     ns2.oldprovider.net.                               skipped — the zone's apex NS records are managed by the platform — importing them would break delegation
dev    NS     1h     ns1.delegated.example.net.                         created
@      SOA    1h     ns1.oldprovider.net. hostmaster.oldprovider.net…   skipped — the zone's SOA record is managed by the platform

5 records — 2 created, 3 skipped
```

Skipping rather than failing is deliberate. Every provider zone-file export contains both records — that is what a zone file *is* — and "migrate my zone off the old provider" is the case this command exists for, so rejecting the flagship input would be absurd. Importing them is worse than either option: merged, the zone advertises Datum's nameservers *and* the old provider's and resolves inconsistently; replaced, delegation to Datum is destroyed and the zone stops resolving. `--replace` does not override the skip.

The `dev` line is the important one. A **non-apex** NS record is a subdomain delegation the user owns, and it is imported normally, even though it lands in the same `<zone>-ns` object as the platform's apex NS records. Apex NS and subdomain NS are the same record type in the same object with opposite ownership. That is why the guard tests the record's shape, and not its type or the object it would land in.

### Why the guard tests shape, not live state

**A guard that asks about live state cannot protect a zone that has none yet.**

The operator creates `<zone>-soa` and `<zone>-ns` only once nameservers have been assigned, so a zone that has not reached that point has no set to classify. A set-based guard returns false there. The write then creates `<zone>-soa` from the old provider's record, under exactly the name the operator later looks for; the operator's existence check finds it and skips creating the real one; and the imported SOA becomes the zone's SOA permanently.

Testing the record's shape asks a question whose answer does not depend on what already exists, so it is immune to the ordering.

The hazard is not specific to `zone import`. It has been found in three commands against the same window: `zone import`, `record apply --prune`, and plain `record apply` — the last of which will write a provider export's SOA and apex NS at exit 0, against a zone the operator has not finished provisioning. Any new write path needs the shape test, not a lookup.

### Compare owner names qualified

> [!IMPORTANT]
> Owner-name identity must be compared qualified, never literally, anywhere the answer gates a destructive action.

Under `--replace`, the platform's existing entries are carried into the new set as a `keep` list, because replacing the type outright would take the apex NS with it. That path shipped broken. The keep list was built from a literal owner-name comparison, so a platform apex NS stored as `example.com.` rather than `@` was not recognised as the platform's. `--replace` dropped both nameserver records while reporting `1 record — 1 created` and exiting 0. The zone stopped resolving and nothing in the output said so. Qualifying both sides through `rdata.FQDN` fixes it.

This was not one mistake. A cross-cutting audit traced **five known instances** to it, each fixed at its own call site before anyone recognised the pattern as one root cause.

**Four were guards that failed open:**

- This keep list dropped the platform's apex NS on `--replace`.
- `record apply --prune`'s `classify` pruned it.
- The apex CNAME-to-ALIAS rewrite did not fire.
- `platformRisk` withheld the warning that editing apex NS can break delegation.

Three of those four had no test for the alternative spelling. `rdata.IsApexIn(name, zone)` now exists so the correct comparison is the easy one to reach for, and bare `rdata.IsApex` documents that using it to gate behaviour is a bug.

**The fifth was not a guard at all**, and it belongs in the list for that reason. `sameRelativeOwner` in `record apply` compared display strings, so an unchanged record was reported as a delete plus an add:

```
+  www                A  300  203.0.113.10
-  www.example.com.   A  300  203.0.113.10
2 changes — 1 to add, 1 to delete
```

The same literal comparison corrupted the artifact the user reads *before* consenting, which under `--prune` turns a display bug into a destructive one. The defect was never confined to protection logic.

Note the range of consequence across the five: two destroyed records, one skipped a rewrite, one merely stayed quiet, and one was loudly wrong. The quiet one is what a review hunting data loss passes over, because **a guard that fails by saying nothing looks exactly like a guard with nothing to say**. The loud one survived for the opposite reason: a diff showing churn reads as the tool being fussy rather than broken.

This is a rule rather than an anecdote because of the failure mode. The guard did not fail because it was missing. It was present, it ran, and it did not recognise its own record. A reviewer checking that the protection exists finds it and moves on.

### `zone import --discover`

Discovery creates a `DNSZoneDiscovery`, polls for the `Discovered` condition, and presents the discovered records for selective import. This is the "migrate my zone off the old provider" path. The discovery status is already relativized with `@` for apex, so it maps straight onto record entries.

Discovery does not return NS, SOA, PTR, or ALIAS.

### `record apply -f <zonefile>`

Apply is the declarative path: diff the file against the live zone and converge, with `--prune` to delete records absent from the file. The diff prints before applying, using compute's diff vocabulary — `→` for changes, `+` and `-` for adds and removes — and a prompt gates the write unless `-y`. Re-applying an unchanged file reports `No changes.`

Platform-managed records are never pruned or modified, and no flag opts into it. Whatever was skipped is reported rather than passed over in silence.

A `--prune` that would delete records is treated as the destructive tier rather than the ordinary one. Where a plain confirmation proceeds when nobody can answer it, a prune with deletions refuses non-interactively and requires `--yes`, because nothing recovers a deleted RRset.

### `zone export`

Export emits a BIND zone file with `$ORIGIN` and `$TTL`, and RFC 1035 TXT chunking for strings over 255 characters, so export, edit, and `record apply` form a closed loop.

## Plumbing

The plugin uses the standard skeleton: a binary named `datumctl-dns`, `plugin.ServeManifest` before Cobra, and `plugin.NewRootCmd("dns", ...)` for the injected `--org`, `--project`, and `--output` flags. `plugin.Token()` is fetched immediately before each call.

`root.SilenceUsage` and `root.SilenceErrors` are both set, because rendering the `Error:`, `Fix:`, and `exit status` block is the plugin's job, and Cobra printing its own version alongside would double it.

### Argument validation is always exit 2

Argument validation is a usage failure everywhere in the tree. Cobra's stock validators — `cobra.NoArgs`, `ExactArgs` — return a plain error that would classify as a generic exit 1, so the root walks the command tree once after registration and re-labels them, leaving any error that already carries an exit code untouched. Doing this centrally rather than per command means reaching for a stock validator is not a way to silently break the contract.

### Persistent flags

Beyond the three SDK flags, the plugin adds the persistent set ipam found necessary: `--verbose`, `--quiet`, `--color`, `--yes`, and `-o name` alongside `table|wide|json|yaml`.

### Entitlement pre-flight

`PersistentPreRunE` runs the service-entitlement pre-flight against the DNS service, with the same non-TTY behaviour compute uses: never hang in CI, and return an error naming `datumctl services enable` instead. It is skipped for `version`, `completion`, `help`, the `__complete*` hooks, any invocation carrying `--help`, and the bare root, which only prints usage.

The service is named two ways, and both are correct in their place. `dns.networking.miloapis.com` is the **service identifier** a user types. `dns-networking-miloapis-com` is the entitlement object's `metadata.name` and `spec.serviceRef.name`. Every user-facing hint uses the identifier.

Recognition also accepts the legacy bare `dns` that compute uses, so a project entitled under an older convention is not asked to enable a service it already has. Two objects can therefore match at once, so the scan takes the **best** phase across all of them rather than the first. A stale rejected grant cannot mask a live one.

### Shell completion

Completion is API-backed for zone names, through `zone list` over the project, and for `--class`, through the cluster-scoped `DNSZoneClass` list. It is static for enum flags and for `--type`, which offers the supported record types rather than the types present in a given zone.

Completion of record names within a zone is **not built**. Nobody has asked for it, and it is listed here so the absence is a decision rather than a gap.

Every path returns `cobra.ShellCompDirectiveNoFileComp`, including error paths, and completion failures are silent. Every API-backed path is bounded by a short deadline, much shorter than the per-request timeout, and returns nothing rather than hanging. A slow command looks slow, but a slow completion looks like a frozen terminal with nothing on screen to say why. The deadline covers the whole operation, including the credentials-helper subprocess, which takes no context and cannot otherwise be interrupted.

### Machine output emits raw objects

`-o json|yaml` emits the raw `DNSRecordSet` and `DNSZone` objects, not the flattened rows. The flattening is a presentation, the same call ipam makes for `pool tree`. Scripts that want the flat view get `-o json` on a future `--flatten`, or use `-o name`.

> [!NOTE]
> Only `--type` narrows the raw output. `--name`, `--status`, and `--managed` are ignored there, and `record list` warns on stderr naming the flags it dropped.

`--type` is a server-side field selector, so it filters whole objects before they are ever fetched. The other three select rows out of the flattened view, and the raw path never builds that view. Honouring them would mean emitting a `DNSRecordSet` with some of its `spec.records` removed — an object the API never served, and one that cannot be applied back. Silently returning a mutilated object is worse than not filtering. A reader who does not know this will file it as a bug, which is why it is written down.

## How this was verified

Two things beyond ordinary unit tests, recorded here because both are cited as evidence elsewhere in this document, and evidence should carry its known failure modes.

### End-to-end harness

`test/plugin` execs the real built binary as a subprocess against a real API server running this repository's real CRDs. A reverse proxy presents envtest at a project control-plane URL, so the production URL construction is exercised rather than bypassed.

The harness pins the exit-code contract, the entitlement pre-flight's non-TTY refusal, and the export-to-apply round trip. That last one failed on its first run and produced the TTL finding in [Compare TTLs by effective value](#compare-ttls-by-effective-value-never-by-pointer).

### Anti-patch mutation testing

Most of the delegation fixes were verified this way: revert the fix, confirm the test now fails, restore. It answers a question ordinary coverage does not — not "does a test exist" but "would the test have caught this" — and it earned its place by finding a test that passed for the wrong reason, aborting before it reached its real assertion.

> [!WARNING]
> "Mutation" here means deliberately breaking the code to check that a test notices. It is unrelated to [Mutation safety](#mutation-safety), which is about writes to the API.

Four rules close ways the **instrument** can be wrong. All four come from failures these harnesses actually exhibited.

1. **Gate on an explicit compile step, separate from the test run.**
2. **Assert the mutation changed the file**, rather than assuming the edit applied.
3. **Match `^--- FAIL`, never bare `FAIL`.** The package-level line is the one that lies.
4. **When the assertion in rule 2 fires, check whether the file moved** before assuming the anchor is wrong.

Rule 3 is the sharp one. For a genuine test failure, `go test` prints both a `--- FAIL: TestX` line and a package-level `FAIL`. For a **compile** error it prints no `--- FAIL` at all, only `FAIL <pkg> [build failed]`. A harness grepping for bare `FAIL` therefore reads "this mutation broke the build" as "the test caught the mutation", and certifies with maximum confidence code that never compiled.

Rule 2 covers the mirror image. A mutation that silently failed to apply reports "nothing failed", which reads as missing coverage that is not missing.

Rule 4 is rule 2's diagnosis rather than a separate check. An anchor carrying tabs or indentation can stop matching because the region was reformatted since the anchor was written, so the failure looks like a bug in the harness when it is really a signal about the code. The distinction matters most for a suite that lives in the repository: without rule 2 at all, a rename or a reformat turns every mutation into a silent no-op and the whole suite reports as bulletproof. Of the failure modes found, **that is the only one that gets worse with time**. It degrades as the code moves, rather than failing on the day it is introduced.

Both directions were observed. One harness manufactured **doubt**, reporting gaps that did not exist. The other manufactured **confidence**, which is far worse, because nobody investigates a passing check.

That is also why this section exists. A technique quoted as evidence is itself a claim, and it needs the same treatment as the claims it supports — including being corrected, repeatedly, by the people relying on it.

> [!NOTE]
> Several errors in this document were caught in the act of writing it. A claim you have to state as a fact is a claim you have to open the file and check, and the checking is free at that moment in a way it never is later.

Three of those errors were paraphrases: a finding that was correct when its author wrote it, restated accurately-sounding by someone relaying it, and wrong by the time it arrived. **A good track record is exactly what makes a claim dangerous to relay unchecked**, because nobody re-derives what a reliable colleague hands them. Cite the primary source rather than restating it. Where a restatement is unavoidable, mark it as one, so the recipient knows to check rather than inherit.

## Phasing

**Phase 1 — the everyday loop. Shipped.** `zone list`, `create`, `describe`, `delete`, and `nameservers`; `record list`, `create`, `set`, `delete`, and `describe`; rdata parsing and client-side validation for all 14 types; status mapping; exit codes; completion; entitlement pre-flight. `datumctl dns version` was added alongside, which the original plan did not include.

**Phase 2 — migration and scale. Shipped.** `zone import` from a file and from discovery, `zone export`, and `record apply -f` with a diff and `--prune`.

**Phase 3 — verification. Half shipped.** `zone nameservers --check` exists and resolves live against the assigned nameservers, answering "is it actually working" rather than "is the control plane happy". **`zone check` is not built.** The command that would run the portal's recommended-setup rules — apex and `www` have an address record, apex has MX — does not exist. It is the only item in the original plan still outstanding.

## Decided since this was written

Three of the original open questions were settled by what shipped. They are kept rather than deleted, so the reasoning is not lost and nobody re-opens them.

1. **`record delete` without an rdata argument deletes the whole RRset.** As proposed, with the count in the confirmation prompt. With a value, only that value is removed. When the last value of a type leaves a zone, the `DNSRecordSet` holding it is deleted rather than left empty, because the API requires at least one entry and an empty set is not a writable state.

2. **`--class` on `zone create` is defaulted, exposed, and completed.** All three parts of the proposal: it defaults to `datum-external-global-dns`, the flag is available, and it completes from the cluster-scoped `DNSZoneClass` list. That class is currently the only one that exists.

3. **`ALIAS` is real and the CLI depends on it.** The question was whether the portal's missing schema member indicated an API gap. It did not. The operator's Go type defines `ALIAS *ALIASRecordSpec`, `rdata` handles the type, and `zone import` rewrites an apex `CNAME` into one. The portal's generated client was stale. Likewise, `status.recordSets[].conditions[]` is served and is now load-bearing: the per-owner-name status column is built on it.

## Open questions

### Is a provenance label on the operator-created SOA and NS sets worth adding?

Still open, and still worth doing.

Identifying the operator's own `<zone>-soa` and `<zone>-ns` sets is a name-and-type heuristic today, and the guards that protect apex NS and SOA records from being overwritten rest on it. A label would replace a guess with a fact, and the portal needs the same signal.

This is the one open question with a correctness consequence rather than an ergonomic one.

### Should `SOARecordSpec`'s numeric fields be pointers?

The CLI half is built; the API gap is a standing request.

`Serial`, `Refresh`, `Retry`, `Expire`, and `TTL` are non-pointer `uint32`, so the backend cannot distinguish "zero" from "unset" and substitutes its default for both: `Refresh=10800`, `Retry=3600`, `Expire=604800`, minimum `3600`, and a serial of `YYYYMMDD01`. A literal `0` is **inexpressible through this API**.

The CLI rejects an explicit `0` with an error naming the default that would have silently replaced it: `the API cannot express a literal 0 for this field — omit it to accept the backend default (…)`. That is the honest behaviour available to a client, and it is shipped.

It does not close the gap. A zone imported from a provider that publishes serial `0` still round-trips to a *different* serial, so for that one field, export and import is not the closed loop `zone export` otherwise is. Any API revision should make those five fields `*uint32`.
