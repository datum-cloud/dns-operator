# Internal DNS API design

Status: Proposed. These examples describe the internal DNS contract, not an
installation recipe or a released API. See [the architecture](README.md) for
control-plane ownership and query serving.

All examples use `dns.networking.miloapis.com/v1alpha1`. The project API assigns
object UIDs and generations. Reference UIDs, timestamps, addresses, and allocated
suffixes below are illustrative. `status` examples show controller output and
are not fields consumers submit when creating resources.

## Ownership and validation

- Trusted VPC integration creates resolver contexts and regional access bindings.
  DNS allocates managed namespaces and reports serving status.
- Project users manage custom private zones, associations, static records, and
  naming policy. Platform policy controls namespace reservations and grants.
- Product publishers write contribution records and eligibility. They cannot
  authorize resolver access, grant themselves publication rights, or write DNS
  service project state.
- DNS owns grant epochs, publication revisions, serving plans, and acknowledgments.
  Admission must enforce field ownership, including fields sharing a status
  subresource; RBAC on that subresource alone is insufficient.

New references pin API-assigned UIDs. References that authorize publication also
pin the policy generation. The authenticated project and source-cluster identity
scope each reference; resource names and self-reported cluster IDs are not
credentials. Cross-project references are outside this proposal.

## Context and regional access

VPC integration creates an opaque DNS scope. DNS does not dereference the
`consumerID` or watch a networking resource.

```yaml
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSResolverContext
metadata:
  name: application
spec:
  # Immutable lifetime identity supplied by trusted VPC integration.
  consumerID: "11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222"
  # Allocate a default private namespace without asking users to choose a zone.
  managedNamespace:
    enabled: true
status:
  # DNS-owned output; publishers use this allocated zone and suffix.
  managedNamespace:
    dnsZoneRef:
      name: managed-application
      uid: 44444444-4444-4444-8444-444444444444
    suffix: vpc-a7c9.project-p4e2.internal
  # DNS-owned fencing epoch for trusted access authorization.
  accessWriterEpoch: 3
  # Regional assignments do not move this object out of the project API.
  servingTargets:
    - region: central
      shard: shared-0
---
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSResolverAccessBinding
metadata:
  name: application-central
spec:
  # Pin the context lifetime so recreation cannot inherit old access.
  contextRef:
    name: application
    uid: 33333333-3333-4333-8333-333333333333
  # Select the serving region, not a separate project control plane.
  region: central
  queryIdentity:
    # Galactic supplies an authorized service-side destination before DNS lookup.
    type: DestinationAddress
    # This is distinct from the well-known address exposed inside the VPC.
    value: fd70:100::10
  # Permit DNS over both transports at the authorized destination.
  port: 53
  transports: [UDP, TCP]
  authorization:
    # Match the DNS-issued context epoch; stale issuers cannot restore access.
    writerEpoch: 3
    # Renewals advance monotonically within this access-binding lifetime.
    sequence: 27
    # Preserve this deadline through transport, replay, and local installation.
    validUntil: "2026-10-08T20:05:00Z"
```

The context reports `Ready` when its namespace and assignment are available.
An access binding reports `Accepted` after authorization validation and `Ready`
after the required serving members apply current access and configuration.
`status.observedSequence` identifies the authorization renewal DNS has observed;
`status.bindingRef` identifies the DNS-owned serving plan.

Context, region, query identity, port, transports, and authorization epoch are
immutable for an access-binding lifetime. Renewals advance the sequence with a
bounded deadline. Destinations must be unique within their routing scope. A
second region gets another binding against the same context. Galactic owns the
consumer-side address, private route, and network lease separately.

## Private zones and associations

Users can add a custom zone to the context and create static records in it.
Private zones do not require public domain verification or public delegation.

```yaml
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSZone
metadata:
  name: production
spec:
  # Visibility is immutable. The existing default remains Public.
  visibility: Private
  # Different contexts can associate separate zones with this same apex.
  domainName: prod.internal
  # Select private backend policy; users do not choose fleet instances.
  dnsZoneClassName: datum-internal-dns
---
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSZoneAssociation
metadata:
  name: production-application
spec:
  # Both objects belong to this project. Pin each object's lifetime.
  dnsZoneRef:
    name: production
    uid: 88888888-8888-4888-8888-888888888888
  # DNS consumes a context reference, not a VPC reference.
  resolverContextRef:
    name: application
    uid: 33333333-3333-4333-8333-333333333333
---
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSRecordSet
metadata:
  name: database
spec:
  # Existing record API: a same-project name reference to its zone.
  dnsZoneRef:
    name: production
  recordType: A
  records:
    # Static records have no endpoint eligibility lease.
    - name: database
      ttl: 30
      a:
        content: 10.20.0.50
```

A context can associate several zones, including its managed zone. Associating
two different zones with the same apex to one context is rejected. Parent and
child zones require deterministic longest-suffix selection. Two contexts can
share one zone through explicit associations, or associate separate zones with
the same apex for isolated answers. Admission must also reject conflicts
between static records and reserved registration names.

## Automatic naming and additional names

DNS owns the lifecycle of the managed namespace and its association. Publishers
read its zone reference from context status. A DNS-owned `DNSManagedNamespace`
can track allocation and cleanup; consumers should not need to create it.

A naming policy adds names in custom zones. It supplements the managed name and
does not require a zone choice on each Compute or Connect resource.

```yaml
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSNamingPolicy
metadata:
  name: application-production-names
spec:
  # Apply naming rules to this DNS scope.
  resolverContextRef:
    name: application
    uid: 33333333-3333-4333-8333-333333333333
  additionalNames:
    # Add instance identity names, such as web-01.instances.prod.internal.
    - registrationClass: InstanceIdentity
      dnsZoneRef:
        name: production
        uid: 88888888-8888-4888-8888-888888888888
      namePrefix: instances
    # Add discovery names, such as api.services.prod.internal.
    - registrationClass: ServiceDiscovery
      dnsZoneRef:
        name: production
        uid: 88888888-8888-4888-8888-888888888888
      namePrefix: services
```

Every target zone must be associated with the context. The proposed classes are
`InstanceIdentity`, `ServiceDiscovery`, `ServiceVIP`, and `ServiceExport`.
Product integrations apply the appropriate rule when reserving names. DNS does
not inspect the underlying product resource to determine its class.

## Product publication

Compute can reserve a name in the managed namespace, obtain a scoped publisher
grant, and publish eligible interface addresses. Connect uses the same contract
with its own publisher principal and export eligibility policy.

```yaml
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSRegistration
metadata:
  name: instance-web-01
spec:
  # Select the managed zone allocated for the resource's DNS context.
  dnsZoneRef:
    name: managed-application
    uid: 44444444-4444-4444-8444-444444444444
  # Reserve this relative owner name, rather than one particular IP address.
  name: web-01.instances
  # Constrain which record types contributions can populate.
  recordTypes: [A, AAAA]
  # Publish only contributions with a current eligible observation.
  publicationPolicy: EligibleContributions
  # Bound client caching; this does not replace the observation deadline.
  ttlSeconds: 30
  # Optional descendant reservations must not overlap other owners.
  reservedDescendants: []
status:
  # DNS-owned output for product status and consumer display.
  canonicalFQDN: web-01.instances.vpc-a7c9.project-p4e2.internal
---
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSContributionGrant
metadata:
  name: web-01-compute
spec:
  # Pin both the registration lifetime and the policy being authorized.
  registrationRef:
    name: instance-web-01
    uid: 55555555-5555-4555-8555-555555555555
    generation: 1
  # Audit label; the authenticated principal below authorizes the writer.
  producerID: compute
  principal:
    # Trusted source-cluster identity distinguishes distributed publishers.
    clusterUID: 99999999-9999-4999-8999-999999999999
    subject: system:serviceaccount:compute-system:dns-publisher
  # Scope the writer to these types and owner names.
  recordTypes: [A, AAAA]
  nameScopes: [web-01.instances]
status:
  # DNS allocates the epoch; the producer cannot choose its own fencing token.
  activeWriterEpoch: 3
  # Identify the grant and registration policy versions DNS accepted.
  observedGrantGeneration: 1
  observedRegistrationGeneration: 1
---
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSRecordContribution
metadata:
  name: web-01-endpoint
spec:
  # Bind the contribution to the reserved name and its current policy.
  registrationRef:
    name: instance-web-01
    uid: 55555555-5555-4555-8555-555555555555
    generation: 1
  # Bind the producer to its authorized grant lifetime and policy.
  grantRef:
    name: web-01-compute
    uid: 66666666-6666-4666-8666-666666666666
    generation: 1
  # Reuse the existing typed record representation.
  recordSets:
    - recordType: A
      records:
        - name: web-01.instances
          ttl: 30
          a:
            content: 10.20.0.10
    - recordType: AAAA
      records:
        - name: web-01.instances
          ttl: 30
          aaaa:
            content: fd20::10
status:
  # Producer-owned: this observation covers the current contribution spec.
  observedGeneration: 1
  # Producer-owned: echo the DNS-issued grant epoch.
  writerEpoch: 3
  # Producer-owned: order observations within that epoch.
  sequence: 18
  # Producer-owned: product policy determines readiness for this name's purpose.
  eligible: true
  reason: NetworkInterfaceProgrammed
  # Producer-owned: DNS must preserve this original freshness deadline.
  validUntil: "2026-10-08T20:01:00Z"
  # DNS-owned: the publication revision that included this contribution.
  publishedRevision: 12
```

Status updates use the current resource version and preserve fields owned by
other controllers. Changing contribution records requires an observation for
the new generation. Admission verifies the authenticated principal, pinned
references, accepted grant epoch, increasing sequence, permitted records, and
bounded freshness interval.

When an endpoint becomes ineligible, its publisher submits a higher-sequence
observation with `eligible: false`. DNS withdraws its addresses through the same
publication path. If the publisher disappears, the original deadline still
expires locally. Recovery requires a fresh eligible observation; replaying the
old one does not restore an endpoint. The product defines whether interface
readiness, application health, or another condition determines eligibility.

The prototype also defines `Persistent`, but its distinct lifetime semantics
need API review. Use `DNSRecordSet` for static records in this proposal.

## DNS service project contracts

These resources are internal to the DNS service project. Consumers and product
publishers cannot write them. Examples show selected fields that explain
coordination; they are not complete generated serving plans or transport payloads.

```yaml
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSResolverBinding
metadata:
  name: application-central
  namespace: dns-platform
spec:
  # Preserve the authenticated source project identity in shared storage.
  projectUID: 11111111-1111-4111-8111-111111111111
  # Prototype wire name: this carries a DNS context UID, not a VPC lookup key.
  vpcUID: 33333333-3333-4333-8333-333333333333
  # Identify the shared regional serving assignment.
  region: central
  shard: shared-0
  # Fence plan replacement separately from configuration changes.
  bindingGeneration: 1
  configurationRevision: 7
  # Trusted node-tier and regional-tier destinations for this context.
  consumerAddress: fd70:100::10
  clusterAddress: fd70:200::10
  port: 53
  transports: [UDP, TCP]
  # Use isolated caches in shared processes, with no private dnsdist packet cache.
  resolverEngine: BIND9
  resolverIsolation: View
  deploymentModel: SharedShard
  dnsdistPacketCache: Disabled
  # Only explicitly associated private zones enter the context's plan.
  zoneUIDs:
    - 44444444-4444-4444-8444-444444444444
    - 88888888-8888-4888-8888-888888888888
  # Preserve the accepted access authorization through every serving tier.
  authorizationIssuerEpoch: 3
  authorizationRevision: 27
  authorizationValidUntil: "2026-10-08T20:05:00Z"
```

The publication resources divide coordination from immutable data:

- `DNSPublicationOwnership` holds the zone's compiler lease, writer epoch,
  revision allocator, and active manifest pointer. Updates use compare-and-swap.
- `DNSPublicationChunk` holds a bounded part of a snapshot with its index, epoch,
  revision, hash, and payload. Chunks are immutable.
- `DNSPublicationManifest` identifies the complete snapshot, its chunks,
  serving targets, and original contribution deadlines.
- `DNSTransportOutbox` records a committed update's subject, identity, hash,
  payload, and dependencies before sending. Ambiguous retries reuse the same
  event identity. Transport acknowledgment is separate from serving acknowledgment.

```yaml
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSPublicationOwnership
metadata:
  name: managed-zone-owner
  namespace: dns-platform
spec:
  # One logical compiler owner per zone lifetime.
  zoneUID: 44444444-4444-4444-8444-444444444444
  # Identify the current controller lease holder.
  holderIdentity: compiler-a
  # Takeover increases the epoch; revisions increase within it.
  writerEpoch: 2
  nextRevision: 13
  # A writer must hold a current lease to commit the active pointer.
  leaseUntil: "2026-10-08T20:01:00Z"
  # The committed snapshot is the activation authority.
  activeManifestName: managed-zone-e2-r12
---
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSPublicationManifest
metadata:
  name: managed-zone-e2-r12
  namespace: dns-platform
spec:
  # Retain the source zone lifetime and its private apex.
  zoneRef:
    name: managed-application
    uid: 44444444-4444-4444-8444-444444444444
  zoneApex: vpc-a7c9.project-p4e2.internal
  # Select this zone's private authoritative variant.
  variant: zone-44444444
  # Prototype wire name: values are DNS context UIDs, not networking references.
  vpcUIDs:
    - 33333333-3333-4333-8333-333333333333
  # Agents reject snapshots behind their accepted writer fence.
  writerEpoch: 2
  revision: 12
  previousRevision: 11
  # Deletion uses a fenced tombstone rather than silently dropping history.
  tombstone: false
  # Verify all chunks before activation; these hashes are illustrative.
  chunks:
    - name: managed-zone-e2-r12-c0
      sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      size: 512
  contentHash: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  generatedAt: "2026-10-08T20:00:00Z"
  contributionFences:
    # Original observation deadlines survive recompilation and replay.
    - uid: 77777777-7777-4777-8777-777777777777
      grantUID: 66666666-6666-4666-8666-666666666666
      epoch: 3
      sequence: 18
      validUntil: "2026-10-08T20:01:00Z"
  # Each required replica receives the complete regional assignment.
  servingTargets:
    - region: central
      shard: shared-0
```

The compiler stages immutable chunks and a manifest, then commits the active
pointer under its current ownership lease. Exporters reconcile that pointer and
repair a missing outbox after a crash. Agents verify identities, hashes, writer
fences, and deadlines before applying a complete snapshot. Acknowledgments must
identify the exact binding or zone lifetime, epoch, and revision applied.
Serving agents retain fences for expired, withdrawn, and deleted state so an
older replay cannot restore it.

## Compatibility and API review

The target integration uses `resolverContextRef` and never requires DNS to read
networking resources. The prototype retains legacy `vpcRef` inputs and internal
`vpcUID`/`vpcUIDs` wire names. Context-based execution treats the latter as opaque
DNS context identities. Rename these internal fields and define migration rules
before stabilizing the API; do not make the old VPC coupling a supported contract.

API review must settle namespace reservation policy, naming conflicts, grant
issuance and revocation, field-level status ownership, deadline bounds, regional
readiness aggregation, snapshot size limits, and the complete internal serving
plan schema. Private visibility and exclusion from public controllers form a
separate implementation boundary in
[DNS operator #230](https://github.com/datum-cloud/dns-operator/pull/230).
