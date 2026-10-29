---
name: delegation-check
description: Use when a Datum DNS zone is Accepted and Programmed but the domain still doesn't resolve, or someone asks whether their registrar nameservers are set up right. Names exactly which nameservers are missing.
---

Call the `dns_guide` tool with `skill: "delegation-check"` and follow the procedure it returns. It is the procedure Datum's own assistant follows, adjusted for the datum-mcp tools available here.

If `dns_guide` is not available, datum-mcp is not running. Tell the person to install it (`brew install datum-cloud/tap/datum-mcp`), make sure the `datum-cloud` plugin is enabled, and restart Claude Code, rather than working the problem without it.
