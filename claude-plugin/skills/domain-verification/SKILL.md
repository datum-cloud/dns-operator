---
name: domain-verification
description: Use when a Datum DNS zone shows PendingDomainVerification or domainLinked false, or a newly created zone isn't doing anything. Explains the domain ownership gate and where the customer clears it.
---

Call the `dns_guide` tool with `skill: "domain-verification"` and follow the procedure it returns. It is the procedure Datum's own assistant follows, adjusted for the datum-mcp tools available here.

If `dns_guide` is not available, datum-mcp is not running. Tell the person to install it (`brew install datum-cloud/tap/datum-mcp`), make sure the `datum-cloud` plugin is enabled, and restart Claude Code, rather than working the problem without it.
