# `datumctl dns` live smoke test

Companion to [`smoke-datumctl-dns.sh`](./smoke-datumctl-dns.sh). The script asserts; this file says what each assertion is for, so a step that starts failing can be judged rather than deleted.

## Running it

```sh
# Everything. Creates a real zone, exercises it, deletes it.
DNS_SMOKE_DOMAIN=zone-i-control.example hack/smoke-datumctl-dns.sh

# Groups 0 and 1 only. Mutates no DNS object, and needs no domain.
hack/smoke-datumctl-dns.sh --read-only

# Skip the bulk paths while zone import/export and record apply are in flight.
hack/smoke-datumctl-dns.sh --domain zone-i-control.example --skip-bulk

# Leave the zone behind for poking at.
hack/smoke-datumctl-dns.sh --domain zone-i-control.example --keep
```

Requires `datumctl`, `go`, `jq` and `python3` on `PATH`, the DNS service enabled on the active project, and a `DNSZoneClass` named `datum-external-global-dns` (the only one that exists today).

> [!IMPORTANT]
> There is no default domain and the script will not invent one. Creating a `DNSZone` claims that domain globally in the platform's accounting store, so a guessed domain does not fail harmlessly — it takes a name away from whoever owns it. Supply a domain you control, via `DNS_SMOKE_DOMAIN` or `--domain`, or run `--read-only`.

Every step is tagged `[READ]` or `[MUTATES]` in its own title, and `--read-only` stops before the first mutation.

> [!NOTE]
> `--read-only` means "creates no DNS object". It is not quite "touches nothing on the server": steps 1.4 to 1.7 all traverse the plugin's `PersistentPreRunE`, and on a project where the DNS service is **not** entitled, with a terminal attached, that pre-flight offers to enable it — and enabling creates a `ServiceEntitlement`. The script closes stdin for every plugin invocation in read-only mode, which makes `util.NonInteractive` true so the pre-flight returns an error instead of offering, but run it against an already-entitled project if you want the guarantee rather than the mitigation. `[MUTATES: attempted]` marks a negative case: it asks the plugin to do something it must refuse, so nothing should change — and the step then asserts that nothing did.

## Outcome vocabulary

| Word | Meaning |
|---|---|
| `PASS` | an assertion held |
| `FAIL` | an assertion did not hold; the step title and the captured output are printed |
| `SKIP` | the step could not run — a subcommand that does not exist yet, or no pty available |
| `PROBE` | an observation with no single correct answer. Both outcomes are legitimate and informative, so neither is a failure. **A green run with probes in it has not checked everything**; read them. |

## Exit status

The script exits **1 if any assertion failed** and 0 otherwise, decided in `cleanup` so that every route out — a clean finish, an early exit, an interrupt — goes through the same check. `SKIP` and `PROBE` do not affect the status; only `FAIL` does. Wiring this into CI is therefore meaningful, which it would not have been while the exit status was a hardcoded 0.

## Cleanup

Teardown runs from a `trap` on `EXIT`, `INT` and `TERM`, so an interrupted run does not strand a zone — and therefore a global domain claim — behind it. It is idempotent, disarms itself against re-entry, and removes the zone *before* the temporary plugin directory, since the deletion dispatches through that directory. If it cannot remove the zone it prints the exact command to run by hand rather than failing quietly.

Two details in there are load-bearing and easy to undo by accident. `set -o pipefail` is on because the teardown pipes its delete through `sed` to indent it — without pipefail the pipeline reports `sed`'s status, and a failed deletion would print "zone removed" and exit clean. And `ZONE_CREATED=1` is set **immediately before** the create request, not after it succeeds: the zone exists server-side the moment the request lands, but `--timeout` keeps the client waiting for nameservers for up to two minutes afterwards, and Ctrl-C during that wait is the most likely way anyone interrupts this script. Setting the flag on success would fire the trap with nothing to clean up. Deleting a zone that was never created is a harmless no-op; the reverse is a stranded domain claim.

## What each group proves

### Group 0 — build and dispatch `[READ]`

That the thing under test is the thing that ships. `make build-plugin` produces `bin/datumctl-dns`; the binary answers `--plugin-manifest` **before** Cobra parses anything, which is the SDK contract that lets the host discover a plugin whose flags it does not understand; and the host dispatches to it out of `DATUMCTL_PLUGINS_DIR`, so the run never depends on what happens to be installed in `~/.datumctl`. `dns version` must work without an entitlement or a project, because a version check that needs a working control plane is useless for debugging one.

### Group 1 — error contract `[READ]`

The exit codes are a stable contract and scripts branch on them, so each distinct failure has to land on its own number rather than collapsing to 1.

- An unknown flag exits **2** with `DNS_USAGE` and a `Fix:` line naming `--help`. Cobra's default would exit 1; `root.SetFlagErrorFunc` is what makes it 2, and this asserts that wiring survived.
- A typo'd noun and a typo'd verb both exit **2** with a nearest-match suggestion. A non-runnable Cobra root prints help and exits **0** by default, which is the wrong answer for a typo in a script — `root.Args` plus `unknownSubcommandError` is what prevents it.
- A nonexistent zone exits **4** with `DNS_NOT_FOUND`, not 1. This is `util.ClassifyError` mapping the API's 404.
- Completion returns `:4` (`ShellCompDirectiveNoFileComp`) and exits 0. A completion path that errors or falls back to filenames corrupts the user's command line, so it must never do either.

> [!NOTE]
> Every negative case in group 4 asserts **exit 2**, not 4. All five record verbs validate the input before they resolve the zone, so malformed input is a usage error even when the zone does not exist. An earlier ordering returned 4 with "zone not found" for these, which hid the real problem behind an unrelated one; if any of them starts returning 4 again, the ordering has regressed rather than the validation.

> [!NOTE]
> The zone positional accepts either the domain or the `DNSZone` object name. Step 2.9 asserts the equivalence, and skips itself when the two happen to be the same string, since there is nothing to tell apart in that case.

### Group 2 — zone lifecycle `[MUTATES]`

`--dry-run` is asserted twice over: it exits 0 and says nothing was created, **and** the subsequent `zone list -o name` confirms the zone really is absent. A dry run that quietly creates something is the worst possible bug in a dry run, and only the second assertion catches it.

Creating the same zone twice must be a conflict rather than a silent no-op, because "already there" and "just made it" are different facts and a script needs to tell them apart.

`zone describe` is asserted to show Class, Status and Delegation, and to name `datum-external-global-dns` — the only class that exists, so any other value means the default went wrong.

> [!NOTE]
> `zone nameservers --check` is a **probe**, not an assertion. It resolves the zone live against its assigned nameservers, and during a smoke run the registrar has not been pointed at them, so "not delegated" is the correct answer. The step asserts only that a verdict was rendered; which verdict is right depends on facts outside the test.

### Group 3 — record happy path `[MUTATES]`

The record surface's load-bearing behaviours.

**`Auto` versus explicit TTL.** An omitted `--ttl` must render as `Auto`, not as `300` and not as a blank — the nil TTL is a distinct state from a TTL that happens to equal the backend default, and flattening the two makes an imported record indistinguishable from a defaulted one. `--ttl 5m` must become 300, and `--ttl 240` must stay **240**: unlike the portal, the CLI does not snap onto a preset ladder, and silently rewriting an imported TTL is the wrong default for a tool people script.

**`create` appends, `set` replaces.** Asserted by counting rows, not by reading a message. `create` twice gives two rows; `set` collapses them to one. A duplicate `create` must fail **and leave both existing values in place** — the count is re-checked after the failure, because "rejected the write" and "rejected the write without damaging anything" are different claims.

**Both notations produce the same record.** An MX entered with `--preference/--exchange` echoes back in presentation format, and `describe` on it shows the named fields; an SRV pasted as `"10 5 5060 sip.<zone>."` round-trips unchanged. Each notation teaches the other, which is the whole reason for having two.

**HTTPS parameters come back in canonical order.** `--param port=443 --param alpn=h3,h2` must serialize as `alpn=h3,h2 port=443`, because `encodeSvcbParams` ranks `alpn` before `port` regardless of input order. If the echo does not match what the backend writes, the echo is lying.

**`--line` and `--dry-run`.** A pasted `dig`-shaped line must yield the name, the type and the TTL from the line itself — the step asserts the record lands at `pasted` with TTL 300 rather than `Auto`, which is what proves the TTL was read out of the line and not defaulted. And `record create --dry-run` must print the `+` diff and create nothing, asserted the same way `zone create --dry-run` is: by re-listing afterwards, because a dry run that quietly writes is the worst possible bug in a dry run.

**A missing value is exit 2.** `record create <zone> novalue A` must fail as a usage error naming what is missing, not reach the API.

### Group 4 — semantic landmines `[MUTATES / negative]`

This is the group worth reading closely. Every case here is a mistake the API server accepts and the DNS backend then mishandles, which is why the CLI has to catch it.

**4.1 — the zone-suffix trap.** `pdns.QualifyOwner` appends the zone to any name without a trailing dot, so `www.example.com` in zone `example.com` becomes `www.example.com.example.com.` The step asserts exit 2, the error naming the real problem, a `Fix:` offering *both* remedies (the bare label and the absolute spelling), and — separately — that no record was created.

**4.2 — a target without its trailing dot.** `qualifyIfNeeded` absolutizes a target by appending a dot and nothing else, so a relative `lb` becomes the root-relative `lb.`, never `lb.<zone>.` There is no spelling of a zone-relative target that behaves the way a user expects, which is why the rule is "always absolute" rather than "we will guess".

Step 4.2b asserts the shape of the suggestion, in both directions. `rdata.requireFQDN` qualifies a bare label with the zone — `mail` → `mail.example.com.`, exactly as the design doc shows — but a value that already contains a dot is a name the user wrote out, so it is only terminated: `lb.example.net` → `lb.example.net.` The step asserts the terminated form appears **and** that the zone-appended form does not, because a `Fix:` line proposing `lb.example.net.example.com.` would walk the user straight into the doubling trap that step 4.1 exists to catch.

**4.3 — multi-value CNAME.** `internal/pdns` keeps the first non-empty CNAME entry and drops the rest, silently. Both shapes of the mistake are covered: a second `create` at the same name, and two values in one `set`. The first case then re-reads the record to confirm the *original* value survived the rejected write.

**4.4 — a value that does not match its type.** Through the CLI the natural form of this mistake is a hostname handed to an `A` record; it must exit 2 client-side, and the existing record at that name must be untouched afterwards.

> [!IMPORTANT]
> **4.4b is the probe that matters most.** It applies a hand-written `DNSRecordSet` with `recordType: A` and a `cname` field as a **server-side dry run**, and reports whether the server accepts it. Server-side dry run runs admission but persists nothing and triggers no reconcile, so it is safe — but the outcome is the load-bearing premise of the whole validation layer. If the server accepts it, the client-side rule is doing real work. If the server *rejects* it, a webhook or CEL rule has landed since this was written, and the design doc's severity callout needs revising. Neither answer is a failure; both need a human to read them.
>
> The reason this matters more than a phantom record: `buildRRSets` contributes nothing for a mismatched entry, so an owner whose only entry is mismatched ends up with a **zero-record rrset**, and `ReplaceRRSetsForRecordSet` converts that into a `DELETE`. A wrong typed field does not merely fail to create — it removes the correct RRset already at that name.

**4.5 — the DKIM case.** A TXT value over 255 bytes. `quoteIfNeeded` wraps `txt.content` in a *single* quoted string unless it is already quoted end to end, so a long value must be stored pre-chunked by `rdata.TXTContentForAPI` or PowerDNS receives a character-string that exceeds the RFC 1035 limit. Three assertions: the write succeeds, the stored `txt.content` unchunks byte-for-byte back to what was written, and it was actually stored as **more than one** character-string. The third is the one that catches a caller who bypassed `TXTContentForAPI` — without it, a short-value test would pass and every real DKIM key would break.

**4.6** covers the other TXT hazard, a semicolon, which is special in presentation format and must come back unescaped in the human view.

**4.7 — deleting the last value of a type.** Removing the last entry must remove the `DNSRecordSet` object, not leave it behind holding zero entries. The type matters: a `DNSRecordSet` holds every record of one type for the whole zone, so the object only goes when the last record *of that type* does. The step therefore uses `AAAA`, which nothing else in the script creates — deleting a lone TXT would leave the DKIM and DMARC records behind, the set would correctly survive, and the step would prove nothing while looking like it passed. It asserts on the CLI's own `record set <name> removed — no AAAA records remain in the zone` line rather than parsing a table column, which is both stronger and not vulnerable to a value containing spaces.

**4.8** asserts the confirmation asymmetry: `record delete` is recoverable, so `ConfirmYesNo` proceeds non-interactively without a prompt; `zone delete` is not, so `ConfirmTyped` refuses. Group 6 asserts the other half.

### Group 5 — bulk paths `[MUTATES]`

These commands now exist, so the guards no longer fire and this group executes for real. It is the least-exercised code in the plugin. Skip it with `--skip-bulk`.

The guard is `require_sub`, which tells three cases apart rather than two. A subcommand that does not exist is a `SKIP`; a subcommand that is present runs; and a `--help` invocation that **itself fails** is a `FAIL`, not a skip. That third case matters: a regression that breaks `dns zone --help` — a panic in `init`, or the entitlement pre-flight starting to fire there — would otherwise convert every guarded step into a skip and leave the run looking clean.

What they check: the export carries `$ORIGIN` and keeps the long TXT value chunked (an export that re-joins it produces a file that cannot be re-imported); applying the zone's own export is a no-op, which is the closed loop `zone export` promises; and an import **reports** an apex `CNAME` rewritten to `ALIAS` and names an unsupported type rather than dropping it silently.

> [!IMPORTANT]
> **5.4 covers a known hazard, not a hypothetical one.** A provider's zone export always carries that provider's apex `NS` and `SOA`. If `zone import` writes them, the zone stops resolving through Datum's nameservers and delegation is destroyed — the worst outcome any command in this plugin can produce, and the exact opposite of what the user asked for by importing. The step captures the apex `NS` and `SOA` before the import, imports a file that deliberately carries a competing set of both, and then asserts the live records are byte-identical to what they were, that the ordinary records in the same file *were* imported, and that no record anywhere in the zone mentions the old provider.
>
> It runs the import **for real** rather than as a dry run, because the guard has to hold on the write path and a dry run would not exercise it. That is safe here only because the zone is created and destroyed by this script and no registrar has ever pointed at it. Do not repoint this step at a zone anyone depends on.

### Group 6 — teardown and the cascade gate `[MUTATES]`

`zone delete` is the highest-blast-radius action in the plugin, so its gate is asserted directly rather than assumed.

- Non-interactively, with the domain piped in and no `--yes`: exit **9**, `DNS_ABORTED`, the message explaining the refusal, the fix naming `--yes` — and then a re-list confirming the zone is still there. `util.NonInteractive` treats a pipe as unanswerable, so piping the confirmation text in does not satisfy the gate; that is the intended behaviour and this asserts it.
- Interactively, under a pty: the cascade warning naming the record count, the "cannot be undone" line, the typed prompt, and an abort when the wrong text is typed.
- With `--yes`: the deletion succeeds and the result states what went with it.

> [!NOTE]
> The interactive step needs a real pty, obtained through `script(1)`, whose flags differ between BSD and GNU. Both shapes are attempted; if neither works the step **skips and records a probe** saying the interactive prompt is unverified by that run. It is worth exercising by hand once, because it is the only gate standing between a typo and a deleted production zone.

## Things that could not be expressed as assertions

- **Delegation state** (2.7). Correct only relative to what the registrar has been told, which is outside the test.
- **Whether the server validates the type/field pairing** (4.4b). Both answers are legitimate; the point is to notice a change.
- **The interactive cascade prompt** (6.2) when no pty is available.
- **Whether `zone import` refuses the platform records or imports around them** (5.4). Both are acceptable behaviours, so only the loose "says something about them" match is asserted there; the strict assertions are on the records being unchanged, which is the part with one correct answer.
- **Programming latency.** Nothing here asserts that a record actually resolves in DNS — only that the control plane accepted it and reports it back. `--wait` and `zone nameservers --check` are the commands that speak to real resolution, and both depend on delegation the smoke run does not have.
