---
name: record-create
description: Use when someone wants to add a DNS record (A, AAAA, CNAME, TXT, MX, SRV, CAA and more) to a Datum DNS zone. Checks for existing owners, renders the record set, and writes only after an explicit yes.
---

Call the `dns_guide` tool with `skill: "record-create"` and follow the procedure it returns. It is the procedure Datum's own assistant follows, adjusted for the datum-mcp tools available here.

If `dns_guide` is not available, datum-mcp is not running. Tell the person to install it (`brew install datum-cloud/tap/datum-mcp`), make sure the `datum-cloud` plugin is enabled, and restart Claude Code, rather than working the problem without it.
