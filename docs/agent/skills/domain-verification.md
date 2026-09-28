# Skill: domain-verification

Use when a zone reports PendingDomainVerification, when `dns_zones_get`
shows `domainLinked: false`, or when a customer asks why a zone they just
created isn't doing anything. This is the earliest gate in DNS: until the
customer proves they own the domain, nothing about the zone is provisioned
at all, and every later check — programming, delegation, individual records
— will look broken for a reason that has nothing to do with them.

The work that clears this happens on a different object, in a different
part of Datum, that this assistant cannot read. Say so plainly and hand
the customer to it rather than guessing at progress.

## Procedure

1. Call `dns_zone_diagnose` for the zone. If the root cause is
   PendingDomainVerification, this skill is the right one and the
   diagnosis already carries how long it has been waiting — use that
   number, don't estimate.

2. Tell the customer what the state actually means, in this order:
   - Their zone is not broken and there is nothing wrong with how they
     created it.
   - Datum has not yet confirmed they own the domain, so it has not
     programmed anything into the DNS backend.
   - This clears by itself the moment verification completes, and only
     then.

3. Point them at where the work happens: the Domain object for their
   domain, which carries its own status and the specific instructions
   they need. Verification is proved either by publishing a value Datum
   gives them at their current DNS provider, or by serving a token over
   HTTP at the domain. Which of the two applies, and the exact value,
   both come from that Domain object — not from anything here.

4. Say what you cannot see. This assistant reads DNS zones and records.
   It cannot read the Domain object's verification status, cannot tell
   them which method was chosen, and cannot read back the value they
   need to publish. Offer to re-check the zone after they've completed
   verification, which is a real and useful next step.

5. If `dns_zones_get` reports `domainLinked: false` and the zone is
   otherwise healthy, the same answer applies: there is no Domain object
   to check against yet. Do not report this as a delegation failure —
   `dns_delegation_check` will say Unknown here, and Unknown means
   nothing has been compared.

6. Once verification completes, the zone moves on by itself. Return to
   `zone-not-resolving` and restart the triage from the Accepted
   condition; don't assume the rest is fine just because this gate
   cleared.

## Do not

- Do not send the customer to their registrar to fix delegation while
  verification is outstanding. Nameservers are not the problem yet, and
  changing them will not clear this. `zone-not-resolving` puts
  verification first for exactly this reason.
- Do not describe a zone waiting on verification as failed, rejected, or
  misconfigured. It is waiting on the customer, and telling them
  otherwise sends them to re-create a zone that is already correct.
- Do not quote a verification value, record name, or token. Those come
  from the Domain object, this assistant cannot read them, and a
  confidently wrong value costs the customer a round trip.
- Do not treat a long wait here as a Datum fault. Verification waits on
  the customer publishing something; elapsed time alone says they
  haven't finished, not that anything is stuck. Report the duration and
  ask whether they've completed the step.
