# DNSSEC

The service does not sign the zones it is authoritative for. There is no key
material, no `DNSKEY`, `RRSIG`, or `NSEC` generation, and no field anywhere in
the API that turns signing on. Every zone the service serves is unsigned, and a
domain delegated to it has to have DNSSEC switched off at its registrar first.

That is a non-goal of the design rather than a feature waiting to be built. This
document records why the service is built that way, what it means for a domain
being delegated, and what would justify revisiting the decision.

## Why zones are unsigned

### A signed zone fails closed, and it fails whole

An unsigned zone degrades. A record is wrong, or missing, and the rest of the
zone keeps answering. A signed zone does not degrade: an expired signature, a
missed key rollover, or a `DS` record out of step with the parent makes every
validating resolver refuse the entire zone, including the signed denials that
say a name does not exist. Mail, certificate issuance, and every subdomain go
with it.

Cached signatures then keep the failure running after the fix is in place, for
as long as the shortest useful TTL says they may. The blast radius of a routine
maintenance slip is the whole domain, and the recovery is not immediate.

### The recurring cost is key management, not signing

Signing a zone once is a backend call. Keeping it signed is a standing
obligation: generating and protecting private keys, scheduling key rollovers so
that old and new keys overlap correctly, and publishing a `DS` record in the
parent zone whenever the key-signing key changes.

That last step is the one that breaks, and it is the one the service does not
control. The `DS` record lives at the registrar, and its correctness is a
property of a system the operator cannot see, cannot write to, and cannot roll
back. A key rollover is only as safe as a third party's ability to publish a
record on time.

### Debugging moves off the query path

An unsigned zone is diagnosed from what the authoritative server was asked and
what it answered. A validation failure is not visible there at all. The
authoritative server answers correctly and the resolver discards the answer, so
the operator has to reconstruct chain-of-trust state from resolvers it does not
run, at the moment the domain is down.

### Multi-provider serving stops being straightforward

Serving one zone from more than one authoritative provider works today because
unsigned answers from either provider are equally valid. Signing removes that
property. Keeping a zone valid across providers means the multi-signer models in
[RFC 8901](https://www.rfc-editor.org/rfc/rfc8901), which require the providers
to share or cross-import key material and to keep their signer sets in step with
each other, on an ongoing basis, through interfaces that mostly do not exist.

A signed zone is therefore a commitment to a single provider in practice, which
is the opposite of what a zone served from several providers is usually for.

## Delegating a domain that is currently signed

Turn DNSSEC off at the registrar and wait for the `DS` records to clear the
parent zone before repointing the nameservers.

Repointing first is the failure this ordering avoids. A `DS` record left in the
parent while the zone is served unsigned tells every validating resolver that
answers must be signed, and they are not. Resolvers read that as an attack and
refuse to resolve the name at all, rather than falling back to the unsigned
answer. The domain goes dark for everyone behind a validating resolver, nothing
in the zone data explains why, and the fix has to happen at the registrar and
then wait out the parent zone's TTL.

## What would justify revisiting

A customer or compliance requirement that names DNSSEC specifically.

Signing changes what the service offers rather than how safely it runs, so the
case for adopting it is a product decision and not an operational one. Reopening
it means answering the four costs above rather than enabling signing in the
backend. The PowerDNS backend can sign a zone today, which is why the absence of
signing here is a decision and not a limitation.

## Related

- [DNS Backends](./backends/README.md) for what the backend layer is responsible
  for.
- [Deployment Topology](./topology.md) for the serving layer that answers
  queries.
