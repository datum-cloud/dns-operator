# Private DNS API contract

This page is the supported contract for Private DNS. It says which resources
each audience writes, which fields they own, the naming rules the service
enforces, what each status means, and what callers can rely on as the API
changes. The CLI, the Portal, and service integrations build on this page.
The [architecture](README.md) explains how the service works and why.

All resources are in `dns.networking.miloapis.com/v1alpha1` and live in a
project namespace.

- [Audiences and resources](#audiences-and-resources)
- [Field ownership](#field-ownership)
- [Naming rules](#naming-rules)
- [Reserved names](#reserved-names)
- [Permissions](#permissions)
- [Status](#status)
- [Limits](#limits)
- [Compatibility](#compatibility)
- [Test coverage](#test-coverage)
- [Known gaps](#known-gaps)

## Audiences and resources

| Resource | Written by | Purpose |
| --- | --- | --- |
| `DNSZone` with `visibility: Private` | Customers, and the DNS service for each network's managed zone | A private namespace, such as `prod.internal` |
| `DNSRecordSet` | Customers | Static records in a private zone |
| `DNSZoneAssociation` | Customers, and the DNS service for each managed zone | Makes a private zone resolvable from one network |
| `DNSResolverContext` | The network integration only | One per network. Names the network's DNS scope and requests its managed zone |
| `DNSResolverAccessBinding` | The network integration only | Connects a network's resolver address in one region to the serving fleet |
| `DNSRegistration` | Approved services only | Reserves a name, or a subtree of names, that a service publishes in a managed zone |
| `DNSRecordContribution` | Approved services only | Publishes one resource's addresses under a registration |
| `DNSNamingPolicy` | Services only, not supported yet | Accepted by the API, but no service applies its rules yet. Outside the customer contract |

Customers read every resource in the table, including the ones they can't
write. The `DNSResolverBinding`, `DNSPublicationManifest`, `DNSPublicationChunk`,
`DNSTransportOutbox`, and `DNSPublicationOwnership` resources belong to the DNS
service's own project. They're internal and outside this contract.

### What customers work with

A customer sees one `DNSResolverContext` per network. The network integration
names it after the network, sets the network as its controlling owner, and
copies chosen labels and a description onto it. Find a network's context by
its owner, not by its name:

```shell
datumctl get dnsresolvercontexts \
  -o custom-columns='CONTEXT:.metadata.name,NETWORK:.metadata.ownerReferences[0].name,ZONE:.status.managedNamespace.dnsZoneRef.name'
```

The context's `status.managedNamespace` names its managed zone and the zone's
domain. Customers can add records to the managed zone outside the reserved
subtrees, create their own private zones, and associate them with a context.

## Field ownership

"Immutable" means the API server rejects a change. To change the value,
delete the resource and create a new one.

| Resource | Field | Owner | Rule |
| --- | --- | --- | --- |
| `DNSZone` | `spec.domainName` | Creator | Immutable |
| `DNSZone` | `spec.visibility` | Creator | Immutable. Defaults to `Public` |
| `DNSZone` | `spec.dnsZoneClassName` | Creator | Required for public zones. Optional and ignored for private zones. Managed zones may carry one |
| `DNSZone` (managed) | `spec`, owner reference | DNS service | Created from the context. The service recreates a deleted managed zone |
| `DNSZone` (managed) | labels and `kubernetes.io/description` | DNS service | Copied from the context. Added or corrected, never removed |
| `DNSRecordSet` | `spec` | Creator | Mutable |
| `DNSZoneAssociation` | `spec.dnsZoneRef`, `spec.resolverContextRef` | Creator | Immutable |
| `DNSResolverContext` | `spec.consumerID` | Network integration | Immutable and unique per project |
| `DNSResolverContext` | `spec.managedNamespace.domainSuffix` | Network integration | Immutable once set. Must end in a permitted private suffix, `internal` by default |
| `DNSResolverAccessBinding` | `spec.contextRef`, `spec.region`, `spec.destinationAddress` | Network integration | Immutable |
| `DNSRegistration` | `spec.dnsZoneRef`, `spec.name` | Service | Immutable |
| `DNSRegistration` | `spec.recordTypes`, `spec.ttlSeconds`, `spec.reservedDescendants` | Service | Mutable. In a managed zone, any change to a registration that declares `reservedDescendants` needs the `reserve` permission |
| `DNSRecordContribution` | `spec.registrationRef` name and UID | Service | Immutable |
| `DNSRecordContribution` | `spec.registrationRef.generation`, `spec.recordSets`, `spec.eligible`, `spec.reason` | Service | Mutable. Records and eligibility change together in one update |
| Every resource | `status` | DNS service | Callers never write status |

References by name are resolved once. The service records the referenced
resource's identity, and a replacement with the same name doesn't inherit the
reference. To attach a replacement zone or context, recreate the association.

## Naming rules

- **Zone domains:** lowercase letters, digits, and hyphens, in at least two
  labels and at most 253 characters. A private zone can use any such domain, including one that also
  exists publicly. On an associated network, the private zone answers for that
  domain and its public records aren't visible.
- **Managed zone domains:** chosen by the network integration, and must end in
  a permitted private suffix, `internal` by default. A managed zone can't shadow
  a public name.
- **Record owner names:** relative to the zone, or fully qualified. `@` is the
  zone apex. Names match `^(@|[A-Za-z0-9*._-]+)$`, and the service compares
  them without case.
- **Parent and child zones:** a network can resolve a zone and a zone below it,
  such as `internal.example` and `db.internal.example`. A query uses the zone
  with the longest matching domain.
- **One zone per domain per network:** two associations can't attach zones
  with the same domain to one network. Different networks can each have a zone
  with the same domain and get independent answers.

### Record rules in private zones

Private zones accept `A`, `AAAA`, `CNAME`, `TXT`, `MX`, `SRV`, `CAA`, `NS`,
`PTR`, `TLSA`, `HTTPS`, and `SVCB` records, with these limits:

- `ALIAS` and `SOA` records aren't supported. The service manages each private
  zone's `SOA`.
- The zone apex can't be a `CNAME`.
- A `CNAME` can't share a name with another record type.
- An `SRV` target must be a name with `A` or `AAAA` records in the same zone.
- A static record without a TTL is served with 300 seconds. The API stores no
  default, so the record set shows no TTL. Records a service publishes
  have a TTL of at most 30 seconds.
- If two record sets claim the same name and type, the older record set keeps
  it. The newer one isn't accepted.
- A customer record set always wins a name over a service registration that
  doesn't reserve it, whatever their ages. Services protect names with
  `reservedDescendants`.
- A `CNAME` target in another zone, or in public DNS, may not be followed. See
  [Known gaps](#known-gaps).
- A record set is accepted or rejected as a whole. One invalid name keeps
  every name in it from being published.

## Reserved names

DNS has no built-in reserved names. A service reserves names by creating a
`DNSRegistration` in a managed zone, and its `reservedDescendants` claim whole
subtrees. Compute reserves `instances`, so every name at or below
`instances.<managed-domain>` belongs to Compute.

While a reservation exists, the service rejects:

- Customer records at or below the reserved subtree.
- A customer zone association whose zone domain would shadow the subtree, such
  as a custom zone for `instances.datum.internal` or for the managed domain
  itself.
- Another registration that overlaps the subtree, from any caller.

Admission rejects these requests with a message that starts with
`ReservedScope:`. The publication compiler applies the same rules, so a request
that reached the API while admission was unavailable is still refused and
reported in status.

## Permissions

Project roles grant these permissions:

| Role | Grants |
| --- | --- |
| DNS Viewer | Read every customer-visible Private DNS resource and its status |
| DNS Admin | DNS Viewer, plus create, update, and delete zones, record sets, associations, and zone discoveries. Registrations and naming policies stay read-only |

Network integrations and publishing services get their own service roles.
Publishing needs two extra permissions on a named `DNSRegistration`:

- `reserve` to create, change, or delete a registration in a managed zone that
  declares `reservedDescendants`.
- `publish` to write a contribution that references such a registration.

A registration without `reservedDescendants`, or outside a managed zone, needs
only the ordinary create and update permissions.

Admission checks both with the caller's own identity. Neither is part of a
customer role.

## Status

Every resource reports standard conditions. A condition is current only when
its `observedGeneration` matches the resource's `metadata.generation`.

### Accepted versus available

Accepting a change and serving it are separate steps. Read them in order:

1. **Accepted:** the service checked the change and included it in the zone's
   next publication. For a record set or registration, read its own `Accepted`
   condition.
2. **Programmed:** at least one resolver applied the zone's publication. Names
   now resolve from at least one place. Read the zone's `Programmed` condition.
3. **Published:** every resolver the zone needs confirmed the publication.
   Read the zone's `Published` condition.

A name a service publishes also reports `Available`, which is true when the
publication holds at least one eligible address for it.

### Conditions by resource

| Resource | Condition | `True` means | Reasons when not true |
| --- | --- | --- | --- |
| `DNSZone` (private) | `Programmed` | A resolver applied the current publication | `PendingReplicaApplication` |
| `DNSZone` (private) | `Published` | Every required resolver confirmed it | `PendingReplicaVerification` |
| `DNSRecordSet` (private zone) | `Accepted` | Included in the zone's publication | `UnsupportedPrivateRecord`, `ReservedScope`, `InvalidRecord`, `InvalidTTL`, `ConflictingTTL`, `NameConflict`, `MissingSRVTargetAddress` |
| `DNSZoneAssociation` | `Accepted` | The zone is attached to the network | `InvalidReference`, `ReservedScope`, `ApexConflict` |
| `DNSResolverContext` | `Ready` | The context and its managed zone are active | `Preparing`, `InvalidIdentity`, `DuplicateConsumerID`, `InvalidManagedDomain` |
| `DNSResolverAccessBinding` | `Accepted` | The access request is valid | `InvalidOrStaleAccess` |
| `DNSResolverAccessBinding` | `Ready` | Resolvers in the region applied this access | `PendingReplicaVerification`, `ObservationUnavailable` (status `Unknown`), `AccessRejected` |
| `DNSRegistration` | `Accepted` | The name is reserved for this service | `ReservedScope`, `NameConflict`, `UnsupportedPrivateRecord`, `InvalidTTL`, `InvalidReservation`, `InvalidReference`, `MissingUID`, `Terminating` |
| `DNSRegistration` | `Published` | Every required resolver confirmed the publication | `PendingReplicaVerification` |
| `DNSRegistration` | `Available` | The name has at least one eligible address | `NoEligibleEndpoints` |
| `DNSRecordContribution` | `Accepted` | Included in the zone's publication | `Ineligible` (withdrawn on purpose), `StaleRegistrationGeneration`, `RetiredContribution`, `NameOrTypeOutsideRegistration`, `MissingSRVTargetAddress`, `InvalidContributionGeneration`, `Terminating` |
| `DNSRecordContribution` | `Published` | Every required resolver confirmed the revision in `status.publishedRevision` | `PendingReplicaVerification` |

`Pending*`, `Preparing`, `Terminating`, and `ObservationUnavailable` clear on
their own. `Ineligible` is a deliberate withdrawal. Every other reason needs
someone to change a resource. A private zone that no network is
associated with isn't published, so it reports no conditions.

New reasons can be added. Follow the rule in
[Conditions and Reasons](../conditions.md#reading-an-unfamiliar-reason): treat
an unknown reason as needing attention, and show its message as written.

### Timing

Publication is asynchronous. An Instance name usually resolves within about
30 seconds of the Instance becoming ready. Clients can keep an old answer until its TTL expires, so a removed or
changed record can still answer for up to its TTL. Callers should wait on
conditions rather than fixed delays.

## Limits

| Limit | Value |
| --- | --- |
| Private zones associated with one network, including the managed zone | 256, not yet enforced. See [Known gaps](#known-gaps) |
| Record types per registration | 14 |
| Reserved subtrees per registration | 64 |
| Values per record type in one contribution | 256 |
| TTL of a service-published name | 1 to 30 seconds |
| Zone domain length | 253 characters |

## Compatibility

`v1alpha1` is a preview API. Within it, changes follow these rules:

- Resource kinds, field names, and condition types listed here keep their
  meaning. A removal or change of meaning is announced in the release notes at
  least one minor release before it ships.
- New optional fields can appear. Callers must ignore fields they don't know.
- New reasons can appear on existing conditions.
- Immutability rules aren't loosened silently. A field that becomes mutable is
  announced in the release notes.
- Schema validation can become stricter to reject values the service never
  served, or values that were accepted but meaningless, such as a public zone
  with an empty class. Each such change is announced in the release notes.
- Closing a gap listed in [Known gaps](#known-gaps) can change which resource
  wins a conflict. Such a change is announced in the release notes.
- A later version, such as `v1beta1`, will be served alongside `v1alpha1` for at
  least one minor release.

Service integrations can also rely on these rules:

- A contribution is published only for the registration UID and generation it
  names. A registration change makes older contributions stale until they're
  rewritten.
- Withdrawal is explicit. A contribution stays published until it's updated
  with `eligible: false` or deleted. A publisher that stops running doesn't
  withdraw its names.
- Resolvers keep serving the last applied publication while the control plane
  is unavailable.

## Test coverage

| Area | Tests |
| --- | --- |
| Schema rules and immutability | `TestPrivateDNSValidationAtAPIServer`, `TestDNSZoneVisibilityAdmission` |
| Live references and admission | `TestCustomerNamedReferencesValidateLiveResources`, `TestAssociationRequiresCurrentResolverContext`, `TestRegistrationRequiresCurrentZone`, `TestUnavailableProjectContextFailsClosed` |
| Reserved names | `TestRecordSetReservedScopes`, `TestAssociationCannotShadowReservedScopes`, `TestAssociationInsideReservedScopesIsNotAccepted`, `TestShadowingWithoutManagedZoneOrRecords` |
| Permissions | `TestServiceReservationAuthorization`, `TestReservationAuthorizationPreservesIdentity`, `TestContributionValidatesReferencesForRBACAuthorizedCallers` |
| Managed zones | `TestManagedDomainRequiresPermittedSuffix`, `TestManagedNamespaceRequiresRequestedDomain`, `TestManagedZoneCarriesContextLabels` |
| Public and private isolation | `TestPublicControllersSkipPrivateZones`, `TestPrivateReferenceRemovesPublicShadow` |
| Reference lifetimes | `TestNamedReferencesPreserveLifetimesAcrossReplacement` |

## Known gaps

These keep the contract from being fully enforced. Each needs its own change.

- **Record set status stops at `Accepted`.** A record set in a private zone
  doesn't report when its names resolve. Callers have to read the zone's
  `Programmed` and `Published` conditions. Record sets should project those
  conditions the way registrations do.
- **Unsupported records fail late.** `ALIAS`, `SOA`, an apex `CNAME`, and an
  `SRV` without a local target pass admission and are rejected only in status.
  Admission should reject them.
- **An unassociated private zone reports nothing.** It should report that it
  isn't associated with any network.
- **Apex conflicts are settled by name.** When two associations attach zones
  with the same domain to one network, the association whose name sorts first
  wins. Record sets use creation order. Associations should too, so a new
  association can't take a domain from an existing one.
- **Naming policies have no effect.** No service applies `DNSNamingPolicy`
  rules yet.
- **Deleting a managed zone breaks the network's names.** The service
  recreates the zone, but keeps the managed association, which still points at
  the deleted zone. The association reports `InvalidReference` and the context
  stays `Preparing`. DNS Admin can delete the zone. The service should replace
  a stale managed association, or refuse deletes of managed zones.
- **The 256-zone limit isn't enforced on associations.** A 257th association
  is accepted, and the network's serving configuration then fails to update.
  Admission or status should refuse it.
- **Cross-zone `CNAME`s can fail over UDP.** When a resolver follows a `CNAME`
  to another zone over UDP with keyed identity, the follow-up lookup loses the
  context identity and is refused. Clients get only the `CNAME`. Lookups over
  TCP, and `CNAME`s within one zone, work. The resolver should keep the
  identity when it follows a `CNAME`.
