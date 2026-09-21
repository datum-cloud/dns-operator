# Skill: zone-not-resolving

Use when a customer says a domain isn't resolving, is showing the wrong
answer, or "isn't working" and you don't yet know which layer is broken.
This is the top-level triage. Follow it in order — do not jump to
records first, because the most common misattribution in DNS support is
blaming a healthy zone for a delegation problem the customer's registrar
caused.

## Procedure

1. Call `dns_zones_get` for the zone. Check whether it has a linked
   Domain and whether that Domain is verified. If verification hasn't
   completed, stop here: nothing else here is provisioned yet, no
   matter what the zone's own conditions say. Load the
   `domain-verification` skill and hand that off — verification happens
   on a different object than the one you're looking at.

2. Check the zone's Accepted condition. If it's False, the zone was
   rejected outright and never reached programming. The message names
   why. This is the end of the triage: nothing about records or
   delegation matters until the zone itself is accepted.

3. Check the zone's Programmed condition. If it isn't True yet, call
   `dns_zone_diagnose` and follow its root cause and next steps. Do not
   move on to delegation until this is resolved — a zone that isn't
   programmed has no live records to delegate to in the first place.

4. Only after Accepted and Programmed are both True does the question
   become delegation. Check `dns_zones_get`'s delegation state:
   - Complete: nameservers match. If the customer still says nothing
     resolves, the problem sits past Datum's own systems — likely
     resolver caching or a stale record answer. Say so plainly; this assistant
     can't see resolver-side caching.
   - Partial or Incomplete: the registrar hasn't fully pointed at
     Datum's nameservers. This is the customer's action to take with
     their registrar, not something Datum can fix. Load the
     `delegation-check` skill for the exact wording of what to tell
     them.
   - Unknown: this means "not yet checked," not "broken." Say that
     plainly rather than sending the customer to their registrar over a
     check that hasn't run. If a zone has stayed Unknown far longer than
     it takes Datum to observe a fresh zone's nameservers, that's worth
     escalating as its own finding, not the same finding as
     Incomplete.

5. Only after all four of the above check out clean does the question
   become "is this specific record correct." Move to
   `record-not-programmed`.

## Do not

- Do not tell a customer to fix delegation before their domain is
  verified or their zone is programmed. The order in this skill exists
  because each earlier gate blocks everything after it; skipping ahead
  produces advice that describes a problem the customer doesn't have
  yet.
- Do not treat Unknown delegation as evidence of a problem. It is the
  honest answer when nothing has looked yet.
- Do not claim a healthy zone that resolves the old answer everywhere is
  necessarily a Datum problem — caching behavior that happens after
  delegation is outside what this assistant can observe, and saying so
  is the correct answer more often than guessing at a cause.
