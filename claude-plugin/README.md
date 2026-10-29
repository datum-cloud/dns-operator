# datum-dns plugin for Claude Code

Lets Claude create and troubleshoot DNS zones and records on Datum Cloud, and
move a domain in from another DNS provider.

## Install

```bash
claude plugin marketplace add datum-cloud/claude-code-plugins
claude plugin install datum-dns@datum-claude-code-plugins
```

## Prerequisites

- The `datum-cloud` plugin, which installs with this one and runs
  [datum-mcp](https://github.com/datum-cloud/datum-mcp) with the DNS tools
  loaded. datum-mcp must be on your `PATH`
  (`brew install datum-cloud/tap/datum-mcp`), and it signs you in through
  your browser on first use.
- For the terminal skills and `/datum-dns:move-zone`, `datumctl` with the dns
  plugin: `datumctl plugin install dns`.

## What's in it

| Skill | Use it when |
|---|---|
| `zone-not-resolving` | A domain isn't resolving and you don't know why. Start here. |
| `domain-verification` | A new zone isn't doing anything yet. |
| `delegation-check` | You want to know whether your registrar points at Datum. |
| `record-not-programmed` | One record isn't live. |
| `record-not-owner` | A record reports `NotOwner`. |
| `conflicting-record` | A record reports `Conflict`. |
| `managed-record-refused` | A record won't change because Datum or a Gateway manages it. |
| `record-create` | You want to add a record. |
| `zone-import` | You're moving a domain from another provider. |
| `manage-dns` | You want to run `datumctl dns` commands yourself. |

`/datum-dns:move-zone <domain> [zone-file]` walks a move from another provider
end to end: zone, DNSSEC check, import preview, import, and nameserver
cutover. Claude asks before every step that writes.

## For contributors

Each triage skill is a short `SKILL.md` that tells Claude when to use it and
asks the `dns_guide` tool for the procedure. The procedures live once, in
[`docs/agent/skills`](../docs/agent/skills), and datum-mcp serves them, so
Claude follows the same steps as Datum's in-product assistant. A skill added
there needs a matching folder here, and a test fails until it has one.
Bump `version` in `.claude-plugin/plugin.json` for users to get a change to
this folder.
