---
status: provisional
stage: alpha
latest-milestone: "TBD"
---

# Internal DNS architecture

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Risks and mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Control-plane boundaries](#control-plane-boundaries)
  - [Contexts and regional access](#contexts-and-regional-access)
  - [Query path and network identity](#query-path-and-network-identity)
  - [Publication and service discovery](#publication-and-service-discovery)
  - [API design](#api-design)
  - [Integration boundaries](#integration-boundaries)
- [Production Readiness Review Questionnaire](#production-readiness-review-questionnaire)
- [Implementation History](#implementation-history)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)

## Summary

Internal DNS lets resources resolve private names within a VPC. Each network has
a managed namespace and can use additional private zones. Product services
publish names automatically, and shared regional fleets serve many VPCs with
isolated DNS contexts.

The [product enhancement](https://github.com/datum-cloud/enhancements/pull/922)
defines the consumer experience. The
[Galactic design](https://github.com/datum-cloud/galactic/blob/main/docs/enhancements/networking/private-service-connect/README.md)
defines private connectivity. This enhancement defines the DNS architecture and
[API design](#api-design).

## Motivation

Resources need private names without a zone choice on every resource. The
platform must isolate overlapping names and addresses across networks and
withdraw endpoints that are no longer eligible.

### Goals

- Serve private zones through the VPC's inherited resolver configuration.
- Allocate default names and support additional custom zones.
- Isolate overlapping names, addresses, and resolver caches across DNS contexts.
- Accept publications from distributed product control planes and withdraw
  endpoints after an explicit health update or deletion.
- Keep serving applied DNS configuration during control plane outages.
- Scale shared serving capacity across regions without deployments per VPC.

### Non-Goals

- Designing Galactic's private connectivity APIs or packet handling.
- Having DNS controllers inspect networking or product resources.
- Defining application health policies for product services.
- Changing public DNS behavior or defining cross-project zone sharing.

## Proposal

Network services selects the default VPC domain, `datum.internal`, and provisions
a DNS context and regional resolver access. DNS allocates the requested managed
namespace, accepts explicit custom zone associations,
and compiles product publications into private serving state. Consumers use a
stable resolver address in their VPC while the platform selects shared capacity.

### Risks and mitigations

- Tenant leakage: authorize network identity before cache lookup, isolate
  resolver views, and test overlapping zones and forged identities.
- Delayed health updates: retain the last reported endpoint state during outages
  and reconcile changes when connectivity returns.
- Tenant identity reuse: retain destination allocations until withdrawal is
  confirmed across the serving fleet.
- Shared fleet failure: qualify configuration activation, readiness, capacity,
  and regional failure handling before release.

## Design Details

### Control-plane boundaries

The DNS service runs in its own VPC. Galactic exposes it through a private
endpoint in each consumer VPC. The diagram shows which API owns each resource;
it does not prescribe where controller processes run.

```mermaid
flowchart TB
  Products[Product services] -->|Records and eligibility| P
  Integration[Trusted VPC integration] -->|DNS access and network intent| P
  P["Consumer project API<br/>DNS resources and network intent"]
  D["DNS service project API<br/>Publication state, fleet plans, and service network intent"]
  K["Karmada API<br/>Placement and propagation"]
  E["Edge API and shared fleet<br/>Local networking, DNS agents, and serving state"]
  P -->|DNS reconciliation| D
  P -->|Network and workload intent| K
  D -->|Service network and workload intent| K
  K -->|Placed desired state| E
  D -->|Committed DNS updates through regional NATS| E
  E -.->|Serving acknowledgments| D
  D -.->|DNS status| P
```

- **Consumer project:** Owns contexts, access bindings, zones, naming policy, and
  publications. VPC integration writes access intent; product services publish
  records and eligibility; DNS reports serving status.
- **DNS service project:** Owns writer leases, snapshots, outboxes, fleet plans,
  and the service's network intent.
- **Karmada:** Places network and workload intent. Guest resolver settings travel
  as desired state; DNS publications use their own delivery path.
- **Edge:** Owns local networking, shared serving workloads, and checkpoints.

DNS uses authenticated project clients. VPC integration and product publishers
translate their resources into DNS intent; DNS does not watch their APIs.

### Contexts and regional access

A logical network has one DNS context across its locations. The context selects
its managed namespace and associated zones. Separate contexts can use identical
zone names and overlapping IP addresses. Sharing a zone requires an explicit
association.

```mermaid
flowchart LR
  C[DNSResolverContext] --> AC[Central access binding]
  C --> AE[East access binding]
  AC --> FC[Central shared fleet]
  AE --> FE[East shared fleet]
```

Bindings live in the consumer project API; `region` selects serving capacity.
Each has independent identity, authorization, and readiness. Publication is
asynchronous, and a failed region must not block healthy regions. Regional access
does not imply that discovery returns only region-local endpoints.

### Query path and network identity

Both VPCs can expose the same DNS address and contain the same client IP. Galactic
authorizes the private path and translates its destination into a distinct
service-side identity. DNS maps that identity to a context before consulting a
cache or selecting private zones.

```mermaid
flowchart TB
  subgraph A[VPC A]
    ClientA[Client 10.0.0.5] --> FrontA[DNS endpoint fd53::53]
  end
  subgraph B[VPC B]
    ClientB[Client 10.0.0.5] --> FrontB[DNS endpoint fd53::53]
  end
  FrontA --> GrantA[Galactic authorized path A]
  FrontB --> GrantB[Galactic authorized path B]
  GrantA --> DestA[Service destination fd70:100::10]
  GrantB --> DestB[Service destination fd70:100::11]
  subgraph Shared[Shared DNS service fleet]
    DestA --> Dist[dnsdist: trusted destination to context]
    DestB --> Dist
    Dist --> ViewA[Context A BIND view and cache]
    Dist --> ViewB[Context B BIND view and cache]
    ViewA --> ZoneA[Private zone in view A: api.prod.internal = 10.20.0.10]
    ViewB --> ZoneB[Private zone in view B: api.prod.internal = 10.20.0.20]
  end
```

Galactic verifies the source VPC lifetime and route authorization before
translating the destination. DNS trusts that path, not client addresses, ECS, or
client-supplied PROXY headers. Consumers cannot select another context by
addressing its service-side destination.

Shared node and regional tiers use:

1. **dnsdist** to select the context before lookup, with private packet caching
   disabled.
2. **BIND views** to isolate positive and negative caches. Node views forward to
   context-specific regional destinations. Regional views host the context's
   private authoritative zones and recurse for other names. Views share resolver
   processes; each context has its own zone selection and cache.

[dnsdist PROXYv2](https://www.dnsdist.org/advanced/passing-source-address.html)
preserves the destination. [BIND PROXY access controls](https://bind9.readthedocs.io/en/v9.20.2/reference.html#namedconf-statement-allow-proxy)
restrict trusted peers and listeners. A shared pool member must have current
configuration and authorization for every assigned context.

[BIND views](https://bind9.readthedocs.io/en/v9.20.2/reference.html#view-block-grammar)
select the private zones visible to each context. Keep caches separate and reject
unmatched destinations before lookup.

### Publication and service discovery

A registration reserves a name and record types. A grant authorizes a product
publisher. A contribution supplies records and an explicit eligibility
observation. DNS compiles accepted contributions into a complete zone snapshot.

```mermaid
sequenceDiagram
  participant Product as Product publisher
  participant Project as Consumer project API
  participant DNS as DNS compiler
  participant Store as DNS service project API
  participant Transport as Regional NATS
  participant Agent as Serving agent
  Product->>Project: Records and eligibility observation
  DNS->>Project: Validate identities, grants, and versions
  DNS->>Store: Stage immutable chunks and manifest
  DNS->>Store: Commit active snapshot under writer lease
  Store->>Transport: Export committed update through durable outbox
  Transport->>Agent: Deliver committed update
  Agent->>Agent: Verify and apply local configuration
  Agent->>Store: Acknowledge applied snapshot
  DNS->>Project: Report serving status
  Product->>Project: Higher-sequence ineligible observation
  DNS->>Store: Commit snapshot that withdraws the endpoint
```

Each zone has one compiler owner, fenced by a compare-and-swap lease. Takeover
advances the writer epoch; revisions increase within it. Agents reject older
versions. The compiler stages immutable chunks and a manifest, then commits its
active pointer. Kubernetes API storage retains this state and a durable outbox;
exporters repair missing outboxes after crashes.

Contexts, access bindings, and publications share one stable shard assignment
per region. Capacity limits admit established contexts before new allocations.
An allocation that exceeds capacity reports a condition without blocking other
contexts or explicit withdrawals. Destination ownership follows the context
lifetime, so recreating its access binding can retain the same destination.

Regional agents render committed snapshots as zone files, validate and activate
them in the associated BIND views, then acknowledge serving.

Regional [NATS JetStream](https://docs.nats.io/nats-concepts/jetstream) delivery is
at least once. Every serving replica receives its complete assignment and
acknowledges applied state separately from broker receipt. Queries use local
state without contacting project APIs or the broker.

Products define eligibility for instance identity, application discovery, or
stable service addresses. Records and resolver bindings persist until an explicit
update or withdrawal. Control plane and messaging outages delay changes but do
not expire applied DNS configuration. Agents can restore verified configuration
from local checkpoints without contacting the control plane.

Agents retain desired version fences and an activated configuration separately.
A rejected expansion retains that configuration; a rejected withdrawal gates
serving. An independent watchdog probes local daemons while updates are staged
and validated. DNS query probes and backend health checks isolate serving
failures from update delivery. Retained tombstones and writer fences prevent replay from
restoring withdrawn records or access. A new serving node requires a complete
configuration before accepting queries.

A reserved name without eligible addresses returns NODATA. A pending private zone returns SERVFAIL only beneath its own apex; other zones
and public recursion continue. Missing required
private state returns SERVFAIL; unknown or withdrawn access returns REFUSED.
Private names never fall back to public resolution. Disable stale private
answers and bound dynamic TTLs; withdrawal budgets include client caching.

### API design

All examples use `dns.networking.miloapis.com/v1alpha1`. The project API assigns
object UIDs and generations. Reference UIDs, timestamps, addresses, and allocated
suffixes below are illustrative. `status` examples show controller output and
are not fields consumers submit when creating resources.

#### Ownership and validation

Customers select zones and contexts by name within their project namespace. DNS
resolves these references and retains their UIDs in controller-owned status.
Internal publication and access contracts pin UIDs and policy generations.
The project API server authenticates callers and uses RBAC to authorize resource
and status operations. Separate roles govern product publication, grant issuance,
network integration, and DNS controllers. Publishers cannot issue grants or
modify resolver authority. Admission validates live DNS references
and the relationship between an authenticated publisher and its current grant.

#### Context and regional access

Network services creates an opaque DNS scope and requests its managed domain.
DNS does not dereference the `consumerID` or watch a networking resource. Other
trusted integrations can request a different domain through the same contract.

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
    # Consumer integration policy selects the apex; it is immutable in this context.
    domainSuffix: datum.internal
status:
  # DNS-owned output; publishers use this allocated zone and suffix.
  managedNamespace:
    dnsZoneRef:
      name: managed-application
      uid: 44444444-4444-4444-8444-444444444444
    suffix: datum.internal
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
    # Order updates monotonically within this access-binding lifetime.
    sequence: 27
```

Access reports `Accepted` after validation and `Ready` after required members
apply current authorization and configuration. Zone publication readiness is
reported separately; a pending zone does not block resolver access. Context, region, query identity,
port, transports, and authorization epoch are immutable per access lifetime.
Updates advance the sequence; destinations are unique within their routing
scope. Galactic owns the consumer frontend and network authorization.

#### Private zones and associations

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
  # Select the zone by name within this project namespace.
  dnsZoneRef:
    name: production
  # DNS consumes a context reference, not a VPC reference.
  resolverContextRef:
    name: application
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

DNS pins each association's resolved zone and context lifetimes. A replacement
with the same name does not inherit the association; recreate the association
to attach it. Customers cannot change the resolved identities in status.

A context can use several zones. Reject duplicate apexes within a context and
conflicts between static records and reserved names. Select parent and child
zones by longest suffix. Separate contexts can explicitly share a zone or use
independent zones with the same apex. Static records have no health lease.

#### Automatic naming and additional names

DNS allocates the requested managed zone and exposes its zone reference and
suffix in context status. Product publishers use these outputs. A naming policy
adds custom names without a zone choice on each Compute or Connect resource.

```yaml
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSNamingPolicy
metadata:
  name: application-production-names
spec:
  # Apply naming rules to this DNS scope.
  resolverContextRef:
    name: application
  additionalNames:
    # Match DNS registration labels from any authorized publisher.
    - selector:
        matchLabels:
          app: storefront
      dnsZoneRef:
        name: production
      namePrefix: storefront
    # An omitted selector adds this name for every registration in the context.
    - dnsZoneRef:
        name: production
      namePrefix: services
```

Every target zone must be associated with the context. DNS retains the policy's
resolved lifetimes; recreate the policy to use a replacement zone or context
with the same name. Selectors match `DNSRegistration.metadata.labels` using
`matchLabels` or `matchExpressions`. An omitted or empty selector matches every
registration in the context. Matching rules add
`<allocated-name>.<namePrefix>.<zone-domain>`; overlapping rules produce one
reservation per zone and name. Product integrations apply the accepted rules
when reserving additional names. Scoped grants authorize publication.

#### Product publication

Compute can reserve a name in the managed namespace, obtain a scoped publisher
grant, and publish eligible interface addresses. Connect uses the same contract
with its own publisher principal and export eligibility policy.

```yaml
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSRegistration
metadata:
  name: instance-web-01
  # Naming policies match these generic registration labels.
  labels:
    app: storefront
spec:
  # Select the managed zone allocated for the resource's DNS context.
  dnsZoneRef:
    name: managed-application
  # Reserve this relative owner name, rather than one particular IP address.
  name: web-01
  # Constrain which record types contributions can populate.
  recordTypes: [A, AAAA]
  # Publish only contributions with a current eligible observation.
  publicationPolicy: EligibleContributions
  # Bound client caching after a record update or withdrawal.
  ttlSeconds: 30
  # Optional descendant reservations must not overlap other owners.
  reservedDescendants: []
status:
  # DNS-owned output for product status and consumer display.
  canonicalFQDN: web-01.datum.internal
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
    # Match the authenticated publisher within this project's API context.
    subject: system:serviceaccount:compute-system:dns-publisher
  # Scope the writer to these types and owner names.
  recordTypes: [A, AAAA]
  nameScopes: [web-01]
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
        - name: web-01
          ttl: 30
          a:
            content: 10.20.0.10
    - recordType: AAAA
      records:
        - name: web-01
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
  # DNS-owned: the publication revision that included this contribution.
  publishedRevision: 12
```

Publishers preserve DNS-owned status fields and observe the current contribution
generation. An ineligible endpoint gets a higher-sequence observation with
`eligible: false`; recovery requires a newer eligible observation. Publisher
unavailability leaves the last reported state in place. Admission checks the
principal, grant epoch, sequence, and record scope.

#### DNS service project contracts

These resources are internal to the DNS service project. Consumers and product
publishers cannot write them. Examples show selected fields that explain
coordination; they are not complete generated serving plans or transport payloads.

The proposed `DNSResolverBinding` groups four concerns: source identity,
regional placement, generated serving configuration, and access authorization.
It binds a consumer DNS context to shared capacity; it does not describe a VPC
or create a resolver deployment.

```yaml
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSResolverBinding
metadata:
  name: application-central
  namespace: dns-platform
spec:
  source:
    # Scope source references to the authenticated consumer project.
    projectUID: 11111111-1111-4111-8111-111111111111
    # Pin the DNS context lifetime; DNS does not dereference a VPC.
    resolverContextRef:
      name: application
      uid: 33333333-3333-4333-8333-333333333333
    # Retain the exact regional access authorization that produced this plan.
    accessBindingRef:
      name: application-central
      uid: aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa
  placement:
    # Identify shared capacity; these fields do not select the source API.
    region: central
    shard: shared-0
  configuration:
    # Fence plan replacement. This is distinct from metadata.generation.
    generation: 1
    # Order configuration changes within this plan generation.
    revision: 7
    listeners:
      # Node-tier service destination, reached through the private network path.
      node:
        address: fd70:100::10
        port: 53
        transports: [UDP, TCP]
      # Regional-tier destination used by the context's node resolver view.
      regional:
        address: fd70:200::10
        port: 53
        transports: [UDP, TCP]
    # Reserve private apices even while their publication is pending.
    zoneApices:
      44444444-4444-4444-8444-444444444444: datum.internal
      88888888-8888-4888-8888-888888888888: production.internal
    zoneRefs:
      # Include only private zones explicitly associated with the source context.
      - name: managed-application
        uid: 44444444-4444-4444-8444-444444444444
      - name: production
        uid: 88888888-8888-4888-8888-888888888888
  authorization:
    # Copy the accepted access epoch and sequence.
    writerEpoch: 3
    sequence: 27
```

Source and zone references resolve in `source.projectUID`, not the DNS service
namespace. Source identity is immutable. The region and node listener match the
access binding; DNS allocates the regional listener.

Configuration generation fences plan replacement; revision orders its updates.
Authorization copies the accepted access epoch and sequence. Serving
acknowledgments identify the binding UID and both version sets; readiness
requires current proof from available capacity in each serving tier.
`FullyReplicated` reports whether every assigned member has applied the plan;
`ObservationHealthy` reports current serving observations. Stale observations
make serving status unknown without withdrawing applied access.

Resolver engine, isolation, deployment model, and dnsdist cache policy belong to
shared fleet configuration.

DNS-owned publication resources retain the compiler lease and active pointer
(`DNSPublicationOwnership`), immutable snapshot data (`DNSPublicationManifest`
and `DNSPublicationChunk`), and durable delivery intent (`DNSTransportOutbox`).
Their storage schemas can be reviewed with the implementation.

#### Compatibility and API review

Schemas, transport, and checkpoints must preserve object lifetimes and ordered
configuration versions. Review naming conflicts, grant revocation, field
ownership, outage recovery, regional readiness, and snapshot limits before
stabilizing the APIs.

### Integration boundaries

Reuse project discovery, authenticated clients, record types, certificates, and
release tooling from
[infra](https://github.com/datum-cloud/infra/tree/main/apps/dns-operator).
Add private admission, compilation, export, and serving agents. Public
LightningStream replication does not provide private snapshot activation.

## Production Readiness Review Questionnaire

Production readiness review remains open. Resolve the following requirements
before release.

### Feature Enablement and Rollback

Define separate alpha controls for publication and serving. Public visibility
remains the default. Revoke access before draining private capacity; dependent
workloads lose resolution. Reenabling requires fresh authorization and observations.

### Rollout, Upgrade and Rollback Planning

Qualify tenant isolation, outage continuity, failover, replay, and upgrade/rollback over
Galactic UDP and TCP paths, including guest resolver configuration. Expand by
context and region after setting withdrawal budgets and tombstone retention.
Compatible upgrades retain transport state and checkpoints. Roll out serving
capabilities before enabling new controller output. Unsupported formats preserve
state and reject activation; conversion must retain authority and withdrawal
fences.

### Monitoring Requirements

Measure query latency/errors, publication lag, outbox backlog, withdrawal delay, and
activation failures. Use bounded metric labels and per-context API status.
Availability, latency, and withdrawal SLOs remain to be defined.

### Dependencies

Project APIs, Galactic/Karmada, NATS, and the DNS fleet support updates and
connectivity. Galactic must retain applied private connectivity during control
plane outages. API or broker outages delay updates while the fleet continues
serving its applied configuration. Health changes and deletions take effect
after delivery resumes.

### Scalability

Measure context capacity, update rates, snapshot size, transport fanout,
cache memory, reload time, and query throughput. Bound chunks and retained
history; establish supported limits and regional failover capacity before release.

### Troubleshooting

Trace project/context/access UIDs through authorization, committed snapshots,
and serving acknowledgments. Inspect network identity for REFUSED, private state for
SERVFAIL, and product eligibility for NODATA.

## Implementation History

2026-10-08: Architecture proposal
[#229](https://github.com/datum-cloud/dns-operator/pull/229) and initial private-zone
boundary [#230](https://github.com/datum-cloud/dns-operator/pull/230).

## Drawbacks

Shared BIND views increase fleet complexity and failure scope. Snapshots and
writer coordination add state and API traffic. During control plane outages,
DNS retains endpoints whose health may have changed.

## Alternatives

PowerDNS Recursor and CoreDNS remain options if they meet the same tenant
isolation and capacity requirements.

## Infrastructure Needed

A DNS service VPC, Galactic private connectivity, shared serving capacity,
protected listener-address allocation, durable project storage, and
secured NATS capacity. Review regional broker placement and failover policy.
