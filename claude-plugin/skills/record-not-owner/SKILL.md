---
name: record-not-owner
description: Use when a Datum DNS record reports NotOwner, meaning another record set already holds that zone, type and name. Finds the competing record set and what has to change.
---

Call the `dns_guide` tool with `skill: "record-not-owner"` and follow the procedure it returns. It is the procedure Datum's own assistant follows, adjusted for the datum-mcp tools available here.

If `dns_guide` is not available, datum-mcp is not running. Tell the person to install it (`brew install datum-cloud/tap/datum-mcp`), make sure the `datum-cloud` plugin is enabled, and restart Claude Code, rather than working the problem without it.
