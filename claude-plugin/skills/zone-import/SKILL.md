---
name: zone-import
description: Use when someone wants to move a domain they already host at another DNS provider onto Datum DNS. Walks the discovery snapshot, the DNSSEC gate, recreating records and the nameserver cutover in a safe order.
---

Call the `dns_guide` tool with `skill: "zone-import"` and follow the procedure it returns. It is the procedure Datum's own assistant follows, adjusted for the datum-mcp tools available here.

If `dns_guide` is not available, datum-mcp is not running. Tell the person to install it (`brew install datum-cloud/tap/datum-mcp`), make sure the `datum-cloud` plugin is enabled, and restart Claude Code, rather than working the problem without it.
