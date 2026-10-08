# Internal DNS architecture

Status: Proposed. The API contracts and serving components require qualification
before release. See [API design](api-design.md) for annotated YAML examples.

Internal DNS lets resources resolve private names within a VPC. One DNS context
selects the names visible to that network, including an automatically managed
namespace and optional custom zones. Many contexts share the same serving fleet;
adding a VPC does not create a resolver deployment.

The [product enhancement](https://github.com/datum-cloud/enhancements/pull/922)
defines the consumer experience. The
[Galactic design](https://github.com/datum-cloud/galactic/blob/main/docs/enhancements/networking/private-service-connect/README.md)
defines private connectivity and network authorization. This document defines
the DNS control plane, publication contracts, and query serving architecture.

## Control-plane boundaries

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

- **Consumer project:** Owns private zones, records, associations, naming policies,
  registrations, grants, and contributions. Trusted VPC integration writes
  context and access specifications; DNS writes their status. Product services,
  such as Compute and Connect, publish their records and eligibility.
- **DNS service project:** Owns durable publication state, writer leases,
  allocation claims, regional serving plans, and the service's network intent.
- **Karmada:** Places network and workload intent at the edge. It is not the DNS
  publication store. Guest resolver settings must travel as desired state in the
  networking projection; source status is not a substitute for that contract.
- **Edge:** Owns local network contexts, interfaces, private endpoints, route
  policies, shared DNS workloads, and serving checkpoints.

DNS controllers use project discovery and authenticated project clients. They
do not watch VPCs, network interfaces, or product resources. VPC integration
translates networking authorization into DNS access. Product publishers translate
resource state into DNS records. These integrations keep networking and product
lifecycles outside the DNS controller.

## Contexts and regional access

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

Both access bindings live in the consumer project's control plane. `region`
selects a serving target; it does not select the API where the object lives.
Each binding has its own UID, trusted destination, authorization deadline, and
readiness. East access can expire while Central access remains available.

Publication reaches regions asynchronously. A region reports readiness after
its required serving members apply the current state. A failed region must not
block updates to healthy regions. Regional access does not, by itself, promise
that service discovery answers contain only endpoints from that region.

## Query path and network identity

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
    Dist --> ViewA[Context A resolver view and cache]
    Dist --> ViewB[Context B resolver view and cache]
    ViewA -->|Context A source marker| Auth[Private authoritative fleet]
    ViewB -->|Context B source marker| Auth
    Auth --> ZoneA[Context A: api.prod.internal = 10.20.0.10]
    Auth --> ZoneB[Context B: api.prod.internal = 10.20.0.20]
  end
```

Client source addresses, ECS, and client-supplied PROXY headers are not tenant
authority. Galactic checks the source VPC lifetime and route authorization before
translating the destination. A consumer must not be able to select another
context by addressing its service-side destination. Galactic handles the return
path to the consumer's DNS endpoint.

The serving candidate uses shared node and regional tiers:

1. **dnsdist** selects the context from the authorized destination. Its private
   packet cache is disabled.
2. **BIND resolver views** isolate positive and negative caches. Node views
   forward to context-specific regional destinations; regional views resolve
   private zones and provide recursive resolution for other names.
3. **PowerDNS Authoritative** serves private zone variants selected by
   service-owned source markers from the regional resolvers.

[dnsdist PROXYv2](https://www.dnsdist.org/advanced/passing-source-address.html)
preserves the destination when forwarding to a shared resolver listener.
[BIND's PROXY access controls](https://bind9.readthedocs.io/en/v9.20.2/reference.html#namedconf-statement-allow-proxy)
restrict that listener to approved service peers and listener addresses. Each
regional resolver replica uses a distinct marker for each context. A shared pool member
is eligible only when all its assigned contexts and authorizations are current.

[PowerDNS views](https://doc.powerdns.com/authoritative/views.html) are experimental
and require the LMDB backend and enabled zone cache. Unmatched clients bypass
view selection, and ordinary variantless zones are implicitly visible to views.
Therefore, private zones must have no variantless copy, and private authoritative
listeners must admit only approved resolver markers.

The original proposal called for PowerDNS Recursor at both resolver tiers. The
prototype has not qualified Recursor's cache isolation for this contract. BIND
views are the current candidate; PowerDNS Authoritative is a different component.
Replacing BIND requires positive and negative cache isolation tests against the
selected Recursor version. Adding a context changes shared configuration and
zone data, not the number of resolver processes.

## Publication and service discovery

A registration reserves a name and record types. A grant authorizes a product
publisher. A contribution supplies records and a time-limited eligibility
observation. DNS compiles accepted contributions into a complete zone snapshot.

```mermaid
sequenceDiagram
  participant Product as Product publisher
  participant Project as Consumer project API
  participant DNS as DNS compiler
  participant Store as DNS service project API
  participant Transport as Regional NATS
  participant Agent as Serving agent
  Product->>Project: Records and fresh eligibility observation
  DNS->>Project: Validate identities, grants, and deadlines
  DNS->>Store: Stage immutable chunks and manifest
  DNS->>Store: Commit active snapshot under writer lease
  Store->>Transport: Export committed update through durable outbox
  Transport->>Agent: Deliver update, including original deadlines
  Agent->>Agent: Verify, apply, and enforce local expiry
  Agent->>Store: Acknowledge applied snapshot
  DNS->>Project: Report serving status
  Product->>Project: Higher-sequence ineligible observation
  DNS->>Store: Commit snapshot that withdraws the endpoint
```

Each zone has one logical compiler owner. Compare-and-swap updates to durable
ownership fence that writer. Takeover advances its epoch; revisions increase
within an epoch. Serving agents reject older epochs and revisions. Controller
leader election alone does not fence a writer after a partition.

Kubernetes API storage holds ownership, snapshots, and outboxes; this design
does not require Postgres. Regional exporters use
[NATS JetStream](https://docs.nats.io/nats-concepts/jetstream) for at-least-once
delivery. Every serving replica receives its complete assigned stream. The
exporter records an outbox before sending and reconciles committed snapshots
that lack an outbox after a crash. A broker acknowledgment confirms delivery to
the broker, not application by a DNS serving member. Queries use local state and
do not contact the broker or project APIs.

Products define eligibility for each record's purpose. An instance identity name
can follow interface readiness; service discovery can follow application health.
A stable service address can remain published while its backend membership
changes. DNS does not infer these policies from product APIs. User-authored
static records have no automatic endpoint health withdrawal.

Contribution freshness and resolver access have separate deadlines. Replay,
restart, or recompilation cannot extend either original deadline. Serving agents
withdraw expired contributions locally; a watchdog removes a member that cannot
enforce expiry. Deletions retain tombstones and writer fences for the supported
replay window.

A reserved name with no eligible addresses returns NODATA. An authorized context
whose required private state is unavailable returns SERVFAIL; an unknown or
expired access identity returns REFUSED. Missing private state must never fall
back to public resolution. Dynamic TTLs are bounded, and stale private answers
are disabled. Withdrawal budgets include publication delay, local expiry, and
client cache TTL: removing a served record cannot erase an answer already cached
by a client.

## Integration and qualification

Reuse project discovery, authenticated project clients, record types, condition
patterns, certificates, release tooling, and PowerDNS/LMDB operating experience.
The [infra configuration](https://github.com/datum-cloud/infra/tree/main/apps/dns-operator)
provides these integration points; its public serving and replication paths do
not establish private tenant isolation.

New components include private API admission, context allocation, publication
ownership and compilation, durable export, shared fleet configuration, and
serving acknowledgments. Private snapshots need their own activation contract;
public zone replication through LightningStream does not replace it.

Before release, validate overlapping zones and addresses, positive and negative
cache isolation, forged identities, UDP and TCP, access revocation, endpoint
withdrawal, writer failover, replay, restart, project API and broker outages, and partial regional
failure. Include Galactic's private path and guest resolver configuration in the
end-to-end environment. Measure fleet capacity and configuration reload behavior
at the expected context count and query rate. Resolve regional failover policy,
broker placement, withdrawal budgets, and tombstone retention before promising
service guarantees.
