---
name: record-not-programmed
description: Use when one DNS record on Datum isn't showing up or looks right but isn't working, after the zone and delegation are ruled out. Reads per-owner-name status and its reason.
---

Call the `dns_guide` tool with `skill: "record-not-programmed"` and follow the procedure it returns. It is the procedure Datum's own assistant follows, adjusted for the datum-mcp tools available here.

If `dns_guide` is not available, datum-mcp is not running. Tell the person to install it (`brew install datum-cloud/tap/datum-mcp`), make sure the `datum-cloud` plugin is enabled, and restart Claude Code, rather than working the problem without it.
