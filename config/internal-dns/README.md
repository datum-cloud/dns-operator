# Internal DNS regional fleet deployment

These manifests show the process boundaries for one shared regional shard.
Replace the example images, addresses, certificates, storage classes, admission
CA injection, and site command wrappers before deployment.

CI publishes `ghcr.io/datum-cloud/internal-dns` for the control plane and
`ghcr.io/datum-cloud/internal-dns-serving` for fleet agents and CoreDNS. dnsdist runs directly from its upstream image.
The serving image includes the validation, activation, and gating
commands in `config/internal-dns/commands`. The Kubernetes qualification
environment uses the same commands.

After both images and the API bundle publish, CI publishes
`oci://ghcr.io/datum-cloud/internal-dns-builds` with the same version tag.
This Flux artifact contains the `internal-dns-images` ConfigMap with both
image manifest digests and the source revision. Staging selects timestamped
feature-branch tags and uses these immutable image references.

The fixed fleet has three roles:

- A `node` resolver agent configures the shared node dnsdist/CoreDNS pair.
- A `regional-dnsdist` resolver agent configures the shared regional frontend and its
  pool of regional CoreDNS members.
- Two or more `regional-coredns` combined agents independently compile the complete
  DNS context snapshot for their member, validate it with
  `coredns -validate`, load it atomically into the context plugin, prove the
  exact version over UDP and TCP, and publish a leased ACK.

Contexts, access bindings, and zones add destinations and record data to
these processes. They never create workloads per VPC or zone. Use `node`,
`regional-dnsdist`, and `regional-coredns` roles for the multi-member production
layout. The `cluster` and `both` roles are limited to development.

Each member needs a distinct member ID, NATS identity and durable consumers,
checkpoint PVC, configuration PVC, and durable activation state. Treat that
set as a single-writer unit. A rolling update must allocate a new member
identity and storage; do not run two pods against one checkpoint.

Kubernetes manages the dnsdist process. An init container creates a private,
pod-local control key and an initial configuration that refuses every context.
The agent validates complete routing snapshots and replaces the rule chain over
the authenticated loopback console. Failed validation retains active rules.
Backend eligibility uses immutable pools shared by identical member sets;
context identity remains the original trusted destination. Listener or backend
topology changes require a replacement member with independent storage. Agent and broker outages retain applied
routing. The control key is generated locally and is never a DNS API field.

`routers.example.yaml` supplies the node and regional frontend workloads.
`fleet.example.yaml` supplies their configurations and the regional CoreDNS
workload template. Give every member independent configuration and checkpoint
storage. CoreDNS process activation uses the serving role; dnsdist runs directly
as its container process.

The agent runs as UID 65532; CoreDNS and dnsdist run as UID 65533. Query
containers do not mount broker credentials and cannot inspect the agent's
filesystem through the shared process namespace. Group 65532 can read accepted
configuration, checkpoints, and the pod-local console key. Only agents that
manage CoreDNS receive `KILL` to stop query processes during activation; no
container receives `SYS_PTRACE`.

## Control-plane and API boundaries


Control-plane replicas discover ready Milo projects through the core API and
watch every namespace in each project control plane. New projects are included
without restarting the service. `projectDiscovery.kubeconfig` authenticates
discovery and project API access; the process `--kubeconfig` authenticates the
DNS service project. Generated kubeconfig Secrets use the `value` key.
Unavailable discovery retains active sources and applied DNS configuration.
Compilation requires the live source project identity to match its worker.
Admission resolves the authenticated project parent against its live identity,
and acknowledgement projection follows the discovered source clients.

Control-plane replicas can concurrently watch the platform API and source
projects. Publication and shard ownership use resource-version checks and
increasing writer epochs on takeover. Each source project has an authenticated
client and optional request limits. An unavailable source must not prevent ACK
projection or publication for healthy sources.

DNS always consumes `DNSResolverContext` and `DNSResolverAccessBinding` from
the project API. It does not watch a networking API. A trusted integration
principal bound to `internal-dns-vpc-integration` creates a context and writes
regional access intent. RBAC separates network integration, product publication, and DNS controller
permissions. Product publishers cannot modify resolver access.
DNS pins the context and access lifetimes in the internal `DNSResolverBinding`
and generates mapping versions. Access persists until an explicit withdrawal.

The context directly owns its requested managed zone and association.
Compute discovers the context status and publishes through
`DNSRegistration` and `DNSRecordContribution`. It does not choose a zone for the managed-name path and receives no NATS credentials.
Publishers write addresses and eligibility together in contribution spec using
optimistic concurrency. DNS owns status and uses API-assigned generations to
reject stale desired state across takeover and replay.

## Serving identity and DNS contexts

Consumer and regional listener prefixes must be local routes in their dnsdist
network namespaces. Galactic authorizes and translates the consumer path to a
service-side destination. dnsdist preserves that destination in a trusted
PROXYv2 header. Node CoreDNS selects a context and forwards to its regional
destination. Regional dnsdist selects the same context and load-balances the
healthy CoreDNS members. Every regional CoreDNS member hosts the context's
private zones. Public queries forward to a shared PowerDNS Recursor service.

An owned name without eligible answers carries service-only `TYPE65280`
metadata at that exact name. This private-use record preserves NODATA without
adding a child name or conflicting with a recovered CNAME. Tenant declarations
cannot use that type. Dynamic registrations require TTLs from 1 to 30 seconds;
client caches can retain withdrawn answers for at most that TTL. Static records
retain their explicit RRset TTL, or the default of 300 seconds. Entries in one
static RRset must use the same TTL.

A context without private zones proves recursion with the root SOA over UDP and
TCP. `verifier.publicName` can override this probe. CoreDNS members need a writable
`/tmp` runtime volume even when the image root filesystem is read-only.

Private packet caching stays disabled in dnsdist and CoreDNS. The context plugin
answers private queries from the accepted in-memory zone data. Endpoint changes
replace that data without retaining cached private answers. Each context has a
separate public cache. PowerDNS Recursor validates DNSSEC for public queries.
Unknown destinations are rejected before cache lookup.

Regional publication proof connects directly to that member's CoreDNS listener,
supplies the exact context destination in a protected PROXYv2 header over both
UDP and TCP, and verifies the installed SOA serial and fingerprint. Restrict
`proxyPeers` to regional dnsdist and the member's local publication agent
network. Local health probes also require the CoreDNS listener address in
`proxyPeers` and loopback in dnsdist ACLs. These peers must remain inside the
protected serving network.

## Transactions, health, and outage recovery

Each attached private apex has independent readiness. A pending zone returns
SERVFAIL while published zones continue serving. Its names cannot fall through
to public recursion or cached answers. If the private apex is unknown, the
context stays gated until its configuration is complete. Completing publication
restores service with the same access mapping. Resolver access readiness
reports the applied path independently of each zone's publication status. Explicit access withdrawal
requires a newer access version to reactivate.

The agent stages a complete JSON snapshot containing context destinations,
private zone data, and upstream destinations. The CoreDNS validator parses every
zone before installation. Atomic rename exposes one complete snapshot; the
plugin builds its replacement state before swapping the lookup table. Invalid
updates retain the accepted state. A record update does not restart CoreDNS.
The agent proves the installed publication over UDP and TCP before acknowledging it.

Publication acknowledgements report verified configuration versions. The fleet
retains accepted proofs during control plane or broker outages and continues
using local DNS probes and backend health checks. Status can report degraded
update delivery without withdrawing an applied resolver binding.

Records and access bindings have no expiry deadline. Product services withdraw
endpoints through explicit health updates or deletion. When delivery stops, DNS
retains the last reported endpoint state. Agents restore applied configuration
from persistent checkpoints before waiting for messaging connectivity. New
nodes require a complete configuration before accepting queries. Replay cannot
regress writer or configuration versions or restore a withdrawn lifetime.

Readiness verifies the approved checkpoint and installed files, then queries
local DNS daemons over UDP and TCP. Healthy daemons continue through normal
agent stops and restarts. Staging and validation can continue while approved
configuration serves. A rejected expansion retains that configuration; a
withdrawal that cannot be applied gates serving. The serving supervisor accepts
approved configuration independently of the agent. Incomplete installation
keeps a durable activation marker; candidate verification requires a live
transaction lock until the approved checkpoint is committed. Orphaned
activation remains gated until recovery verifies the saved configuration.
Readiness and activation gates use local state without Kubernetes or NATS.

Mount the transaction stage, active CoreDNS configuration, checkpoint,
and activation state on storage local to the serving host. Replication uses
the durable publication stream and independent regional members; each member
has its own crash-safe checkpoint volume.

The transaction stage and active configuration must share one filesystem:
installation uses atomic rename. The examples put the stage at
`/etc/internal-dns/stage` on the configuration PVC and keep checkpoints on the
separate state PVC.

## Capacity and transport

Set `maxBindings` from measured CoreDNS context memory, snapshot activation time,
dnsdist rule cost, NATS payload size, and activation recovery time. The example caps a
shard at 200 contexts. Add another fixed shard before
raising those bounds.

NATS uses TLS with a verified server name, client certificate, and one scoped
identity per process. Control-plane credentials publish serving/publication
subjects and consume ACKs. Each serving member can consume its assigned durable
subjects and publish only its own hashed ACK subject. Broker partitions delay
updates while the fleet continues serving its applied configuration.

`fleet.example.yaml` provides node, regional frontend, and regional CoreDNS JSON
configurations plus one Deployment template. Instantiate the template for each
member and create a second regional CoreDNS member with distinct addresses and
storage. `control-plane.example.yaml` lists both regional members in
`clusterBackends` and `members`.

## Resource access

The project API server authorizes DNS resource and status operations. Bind the
roles in `rbac.yaml` to service principals in each project namespace. Customer
access uses the protected resources and DNS roles in `config/iam`.

The webhook validates live resource relationships. Project RBAC controls who
can publish; the compiler validates registration ownership, record types, and
observation versions. Publishers preserve DNS-owned publication status during
optimistically locked observation updates.

## Rollout and recovery

Project APIs select zones by name and report names and readiness. DNS stores
resolved ownership identities in reference ConfigMaps in the service project.
The collector checks source resources before removing those records.

The supported envelope version is `v1alpha6` and checkpoint format is 7. Keep
reference records, transport state, and persistent checkpoints across compatible upgrades. Deploy
serving agents before controllers that emit per-zone pending state. Enable the
controller change after every serving member supports that contract; a matching
wire version alone does not establish feature compatibility.

The checkpoint retains desired version fences and the activated configuration
separately. Restart can restore the activated configuration during a rejected
expansion or broker outage. Explicit withdrawal invalidates that fallback before
installation. Unknown checkpoint or wire formats preserve files and reject
activation. Format conversion requires a tested migration that retains lifetime,
withdrawal, configuration, and publisher fences; never reset those fences to
complete an upgrade.

Snapshot budgets admit established bindings before new allocations. Overflow
reports `CapacityAvailable=False` for the affected binding while healthy
contexts and withdrawals continue. A pending zone fails only queries beneath
its private apex; its names never fall through to public recursion. Bindings,
publication targets, and context status use the same stable shard assignment.

### Control-plane monitoring

Set `metricsBindAddress` to an internal listener, such as `:8080`, to expose
`/metrics` on the control-plane process. The endpoint is disabled when the
setting is omitted or set to `0`. Restrict access to the monitoring network.

The endpoint includes controller-runtime reconciliation metrics and:

- `internal_dns_operations_total{operation,result}`: completed operations,
  with `result` set to `success` or `error`.
- `internal_dns_operation_last_success_timestamp_seconds{operation}`: the
  latest successful completion time. Errors do not advance this timestamp.

Operations identify configured discovery, planner, acknowledgement refresh,
transport outbox, and acknowledgement processing loops. A successful loop
can be idle; it does not prove that an endpoint is serving. Combine these
metrics with binding conditions, broker backlog, and DNS queries from a VPC.

### Reference-state recovery

An observed association, naming policy, or registration requires its private
reference state to retain the target object's identity. If that state is
missing, DNS rejects the source instead of attaching it to a same-name target.
The rejection persists across retries. Restore a verified reference-state
backup, or recreate the source resource to select its targets explicitly.

Registration write permission is the publication boundary within a project.
Naming-policy selectors organize names; they do not authorize publishers.
A publisher that can change registration labels can select every matching
rule for the registration's associated context. Restrict registration writes
with RBAC when publishers must not share that authority.
