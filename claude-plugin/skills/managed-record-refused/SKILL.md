---
name: managed-record-refused
description: Use when someone wants to edit or delete a Datum DNS record, or asks why a change won't stick, and the record is managed by a Gateway, connector, external-dns or the platform. Explains where the real edit belongs.
---

Call the `dns_guide` tool with `skill: "managed-record-refused"` and follow the procedure it returns. It is the procedure Datum's own assistant follows, adjusted for the datum-mcp tools available here.

If `dns_guide` is not available, datum-mcp is not running. Tell the person to install it (`brew install datum-cloud/tap/datum-mcp`), make sure the `datum-cloud` plugin is enabled, and restart Claude Code, rather than working the problem without it.
