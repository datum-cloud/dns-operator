---
name: zone-not-resolving
description: Use when a domain on Datum DNS isn't resolving, returns the wrong answer, or "isn't working" and the broken layer is unknown. Top-level DNS triage that checks verification, zone, delegation, then records in order.
---

Call the `dns_guide` tool with `skill: "zone-not-resolving"` and follow the procedure it returns. It is the procedure Datum's own assistant follows, adjusted for the datum-mcp tools available here.

If `dns_guide` is not available, datum-mcp is not running. Tell the person to install it (`brew install datum-cloud/tap/datum-mcp`), make sure the `datum-cloud` plugin is enabled, and restart Claude Code, rather than working the problem without it.
