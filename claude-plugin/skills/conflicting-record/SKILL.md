---
name: conflicting-record
description: Use when a Datum DNS record reports Conflict, such as a CNAME clashing with another record at the same name. Tells a customer-fixable conflict apart from one only Datum can clear.
---

Call the `dns_guide` tool with `skill: "conflicting-record"` and follow the procedure it returns. It is the procedure Datum's own assistant follows, adjusted for the datum-mcp tools available here.

If `dns_guide` is not available, datum-mcp is not running. Tell the person to install it (`brew install datum-cloud/tap/datum-mcp`), make sure the `datum-cloud` plugin is enabled, and restart Claude Code, rather than working the problem without it.
