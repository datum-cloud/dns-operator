# Reserved managed-zone scopes

The control plane's required top-level `reservedScopes` list reserves relative
subtrees in every resolver context's managed private zone. Reservations exist
before any registrations, contributions, or service records. DNS enforces the
policy using the existing resolver context, managed zone, registration, and API
authorization mechanisms; it introduces no CRD.

A platform deploying compute and database services can configure:

```json
{
  "reservedScopes": [
    {"name": "instances", "publishers": ["system:serviceaccount:compute-system:dns-publisher"]},
    {"name": "databases", "publishers": ["system:serviceaccount:database-system:dns-publisher"]}
  ],
  "admission": {
    "enabled": true,
    "certDir": "/var/run/internal-dns/admission"
  }
}
```

These are platform policy examples, not built-in DNS reservations. Use the
actual authenticated usernames seen by the project API, including impersonation
used by publishers. Exact username membership does not grant API permissions or
replace registration coverage and lifetime validation.

Names must be lowercase relative DNS subtrees, without a trailing dot, wildcard,
or `@`. Multi-label subtrees are supported. Overlapping or duplicate reservations
are rejected. Matching is case insensitive and respects label boundaries:
`web.instances` and `instances` match; `myinstances` and `instances.web` do not.
Absolute names with or without a trailing dot match the same way as relative
names in the private-zone compiler.

## Enforcement

- DNSRecordSet admission and compilation reject reserved owners with
  `ReservedScope`. Even authorized publishers use registrations/contributions:
  static record sets carry no publisher identity.
- DNSRegistration admission checks its owner and reserved descendants, including
  descendant claims that contain a configured subtree.
- DNSRecordContribution admission checks the authenticated username alongside
  existing registration coverage, type, and lifetime validation. As before,
  stored objects cannot prove the authenticated caller; install this webhook
  with `failurePolicy: Fail`. Audit existing contributions when changing the
  publisher list: changing this list does not itself revoke stored contributions.
- DNSZoneAssociation admission and reconciliation reject tenant zones at/below
  a reserved root, between a multi-label reservation and the managed apex, or at
  the managed apex itself. Ancestors above the managed apex remain usable because
  the more specific managed zone wins. The check uses the resolver context's
  declared `managedNamespace.domainSuffix` even before its managed zone exists,
  and also protects the resolved suffix. It applies only in that resolver context.
- Tenant-owned private zones, public zones, and names outside reserved scopes
  retain their existing behavior. Copying a managed owner reference onto a
  differently named tenant zone does not turn it into a managed zone.

DNSRecordSet scope admission now runs at `/validate-internal-dns` alongside the
other policy checks, with the same configuration as reconciliation. Include
`dnsrecordsets` CREATE/UPDATE in its failure-closed webhook rules. Record-set
status remains outside that webhook. The public DNS operator keeps its normal
record validation. Its unconfigured internal-DNS handler refuses managed-zone
writes instead of silently dropping protection; protected deployments must use
the configured internal-DNS control-plane endpoint, including in kind/e2e setups.
Do not register both endpoints for the same resources.

## Empty configuration and migration from PR #338

- Missing or `null` `reservedScopes` fails control-plane startup. An explicit
  `reservedScopes: []` deliberately reserves nothing.
- Missing or empty `publishers` denies all publishing in that subtree while
  retaining static-record and shadowing protection. This differs from PR #338's
  empty `instancePublishers`, which allowed any API-authorized publisher.
- Nonempty reservations require `admission.enabled: true`. Agent roles do not
  need this policy. Restart all control-plane replicas after configuration changes.
- `admission.instancePublishers` is recognized only to fail startup with migration
  guidance, including empty/null values and configurations containing both fields.
  There is no implicit compute policy and no silent translation.

1. Inventory existing records, registrations, contributions, and associations.
   Replace `instancePublishers` with an explicit `instances` reservation and copy
   its usernames into `publishers`. If the old list was empty, enumerate legitimate
   publishers first. Resolve existing objects that the new policy would reject.
2. Prepare the binary and complete policy together in a versioned deployment.
   Preserve `instances`; never use `[]` as a temporary compatibility setting.
   Old strict decoders cannot read the new field, so keep version-matched configs.
3. Stage ready admission endpoints with the new policy, then switch the
   failure-closed webhook service/rules, including `dnsrecordsets`, to them.
   Keep the old record-set guard until the new one is active. Do not mix old,
   policy-blind, or unconfigured endpoints into the service, particularly when
   introducing additional scopes.
4. Roll reconciliation replicas to the same policy and verify admission denials,
   `ReservedScope` association conditions, and publications. Startup failures are
   configuration errors to fix, never a reason to remove policy.
5. A rollback must preserve equivalent enforcement. PR #338 protects only
   `instances`; it cannot safely preserve new service reservations by itself.

See the [configuration example](../../../config/internal-dns/control-plane.example.yaml)
and [webhook rules](../../../config/internal-dns/admission.example.yaml).
