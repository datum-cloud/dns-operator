# Private DNS regional fleet deployment

The [example manifests](../../config/internal-dns/) show the process boundaries
for one shared regional shard.
Replace the example images, addresses, certificates, storage classes, admission
CA injection, and site command wrappers before deployment.

CI publishes `ghcr.io/datum-cloud/internal-dns` for the control plane and
`ghcr.io/datum-cloud/internal-dns-serving` for fleet agents and CoreDNS. dnsdist
runs directly from its upstream image.
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
- A `regional-dnsdist` resolver agent configures the shared regional frontend
and its
  pool of regional CoreDNS members.
- Two or more `regional-coredns` combined agents independently compile the
complete
  DNS context snapshot for their member, validate it with
  `coredns -validate`, load it atomically into the context plugin, prove the
  exact publication version over TCP, and publish a leased acknowledgment.
  Resolver-path probes separately check the consumer-facing path over UDP and
  TCP.

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
topology changes require a replacement member with independent storage. Agent
and broker outages retain applied
routing. The control key is generated locally and is never a DNS API field.

[Router manifests](../../config/internal-dns/routers.example.yaml) supply the
node and regional frontend workloads.
[Fleet manifests](../../config/internal-dns/fleet.example.yaml) supply their
configurations and the regional CoreDNS
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
and acknowledgment projection follows the discovered source clients.

Control-plane replicas can concurrently watch the platform API and source
projects. Publication and shard ownership use resource-version checks and
increasing writer epochs on takeover. Each source project has an authenticated
client and optional request limits. An unavailable source must not prevent ACK
projection or publication for healthy sources.

DNS always consumes `DNSResolverContext` and `DNSResolverAccessBinding` from
the project API. It does not watch a networking API. A trusted integration
principal bound to `internal-dns-vpc-integration` creates a context and writes
regional access intent. RBAC separates network integration, product publication,
and DNS controller
permissions. Product publishers cannot modify resolver access.
DNS pins the context and access lifetimes in the internal `DNSResolverBinding`
and generates mapping versions. Access persists until an explicit withdrawal.

The context directly owns its requested managed zone and association.
Compute discovers the context status and publishes through
`DNSRegistration` and `DNSRecordContribution`. It does not choose a zone for the
managed-name path and receives no NATS credentials.
Publishers write addresses and eligibility together in contribution spec using
optimistic concurrency. DNS owns status and uses API-assigned generations to
reject stale desired state across takeover and replay.

## Serving identity and DNS contexts

The control plane leases each resolver context a regional address from
`clusterPrefix`, which the regional dnsdist routes locally. With more than one
region, give each region's shards their own `clusterPrefix` so a region's node
resolvers forward to its own regional tier. Regions without one share the
top-level pool and the context's one address, so adding a pool later never
readdresses existing bindings. Pools must not overlap, and a region's pool
cannot be renumbered once leased from. A region that gains a pool keeps its
existing bindings on shared-pool addresses, so it must route the shared pool to
its own regional tier as well until those bindings are gone. A new pool also
refuses any service destination inside it, including ones already accepted, so
choose pools clear of the destinations integrations use.

Consumer and regional listener prefixes must be local routes in their dnsdist
network namespaces. Galactic authorizes and translates the consumer path to a
service-side destination. dnsdist preserves that destination in either a
PROXYv2 header or a keyed private EDNS option. Configure the same identity mode
at both ends of each hop: setting `identityKeyFile` selects the EDNS mode;
omitting it selects PROXYv2. CoreDNS accepts traffic only from configured peers
and validates the key in the EDNS mode. Keyed members listen on UDP and TCP;
members without a key listen on TCP only. In the default forwarding mode, node
CoreDNS selects a context and forwards to its regional destination. Regional
dnsdist selects the same context
and load-balances the healthy CoreDNS members. Every regional CoreDNS member
hosts the context's private zones. Public queries forward to a shared PowerDNS
Recursor service.

As an opt-in migration, a node agent may use `mode: "combined"` with
`nodeAuthoritative: true` and `render.role: "node"`. Node CoreDNS then embeds
the same validated publications instead of forwarding private zones to the
regional destination. Regional control planes still compile and publish the
records, and regional serving stays in place until the node fleet is validated.
Set `publicationDNSProbe.server` to that node's protected CoreDNS listener and
configure `render.nodeCoreDNS.publicUpstreams` for public recursion. Verify both
before enabling the option. Node publication proof uses the consumer context
destination; regional proof uses the regional destination.

Node publication acknowledgments do not contribute to regional publication
readiness. Use node metrics and consumer queries to verify local progress.
See [Node-local Private DNS authority](../enhancements/node-local-private-dns-authority.md)
for bootstrap validation, migration, and rollback.

An owned name without eligible answers carries service-only `TYPE65280`
metadata at that exact name. This private-use record preserves NODATA without
adding a child name or conflicting with a recovered CNAME. Tenant declarations
cannot use that type. Dynamic registrations require TTLs from 1 to 30 seconds;
client caches can retain withdrawn answers for at most that TTL. Static records
retain their explicit RRset TTL, or the default of 300 seconds. Entries in one
static RRset must use the same TTL.

A context without private zones proves recursion with the root SOA over UDP and
TCP. `verifier.publicName` can override this probe. CoreDNS members need a
writable
`/tmp` runtime volume even when the image root filesystem is read-only.

Private packet caching stays disabled in dnsdist and CoreDNS. The context plugin
answers private queries from the accepted in-memory zone data. Endpoint changes
replace that data without retaining cached private answers. Each context has a
separate public cache. PowerDNS Recursor validates DNSSEC for public queries.
Unknown destinations are rejected before cache lookup.

Regional publication proof connects directly to that member's CoreDNS listener,
supplies the exact context destination in a protected PROXYv2 header over TCP,
and verifies the installed SOA serial and fingerprint. This publication check
uses TCP even when the member also supports UDP. Separate resolver-path probes
check UDP and TCP through dnsdist. Restrict
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
reports the applied path independently of each zone's publication status.
Explicit access withdrawal
requires a newer access version to reactivate. Unknown access returns REFUSED;
withdrawn access returns REFUSED after the member applies the withdrawal.

The agent stages a complete JSON snapshot containing context destinations,
private zone data, and upstream destinations. The CoreDNS validator parses every
zone before installation. Atomic rename exposes one complete snapshot; the
plugin builds its replacement state before swapping the lookup table. Invalid
updates retain the accepted state. A record update does not restart CoreDNS.
The agent proves the installed publication directly over TCP before
acknowledging it. Resolver-path probes check both UDP and TCP separately.

With `identityKeyFile` configured, CoreDNS listens on both UDP and TCP at its
management address. Both transports accept only `proxyPeers`; each UDP query
must additionally carry a valid keyed destination option. Members without an
identity key retain the TCP-only PROXYv2 listener. The key is a shared secret,
not encryption: keep this hop on the trusted serving network.

To try UDP between dnsdist and CoreDNS, set `backendTransport: "udp"` in
`render.nodeDNSDist` or `render.clusterDNSDist`. The default is `"tcp"` (including
when omitted). UDP mode sends UDP client queries over UDP and keeps TCP for TCP
clients, including retries of truncated answers. It requires `identityKeyFile`.
Deploy the UDP-capable CoreDNS image first, permit UDP on the backend port (5300
in the example) from the configured peers, then replace the dnsdist member with
the new setting. A transport change requires replacement; a rules reload cannot
change existing backend connections. Restore `"tcp"` and replace the dnsdist
member to roll back. Keyed CoreDNS readiness checks both transports.

For a fleet with forwarding nodes, enable keyed UDP on regional CoreDNS and
regional dnsdist before enabling it on node members. The node CoreDNS forwarder
uses the incoming query's transport upstream, so node UDP also makes the traffic
arriving at regional dnsdist UDP. Leaving that regional front in TCP-only mode
moves UDP-to-TCP conversion to the regional tier. In staging load tests this
mixed configuration saturated regional dnsdist and exhausted its memory even
though the node listener had spare CPU. Mount the shared regional identity key
in both the dnsdist sidecar and its agent, and in each regional CoreDNS member.
Roll back node members before returning the regional tier to TCP-only mode.

Transport rollback should retain the identity keys and keyed TCP configuration.
Changing between legacy PROXYv2 and keyed identity is a separate fleet migration:
keyed CoreDNS rejects unkeyed readiness canaries even though it can still serve
trusted PROXYv2 TCP queries. Do not wait for an unkeyed frontend to become ready
against an entirely keyed backend fleet. Use a separately ready regional fleet
for the identity-mode cutover; the transport rollout order above assumes the
regional identity configuration is already compatible.

For keyed UDP listeners, `render.nodeCoreDNS.udpReadBufferBytes` and
`render.clusterCoreDNS.udpReadBufferBytes` request a socket receive buffer in
bytes. Omitted or zero preserves the operating system default; negative values
are rejected. Changing this setting requires a CoreDNS restart. This controls
queued datagrams, not the maximum DNS response payload. Linux may clamp the
request to `net.core.rmem_max` and reports twice the requested size for socket
accounting. Startup logs report the requested and effective sizes. Compare
socket drops, successful-answer tail latency, CPU per successful query, and
memory when testing larger buffers: buffering can replace drops with queueing.

Compare query outcome/latency metrics with
`internal_dns_transport_datagrams_total{outcome="accepted"}` and
`internal_dns_transport_datagrams_total{outcome="untrusted_peer"}`. These counters
count datagrams before DNS parsing and identity validation; accepted datagrams
are not necessarily successful queries. TCP admission counters remain available
for TCP client traffic and fallback.

Publication acknowledgments report verified configuration versions. The fleet
retains accepted proofs during control plane or broker outages and continues
using local DNS probes and backend health checks. Status can report degraded
update delivery without withdrawing an applied resolver binding.

Records and access bindings have no expiry deadline. Product services withdraw
endpoints through explicit health updates or deletion. When delivery stops, DNS
retains the last reported endpoint state. Agents restore applied configuration
from persistent checkpoints before waiting for messaging connectivity, and
gate serving if a validated bootstrap does not complete within 15 minutes of
the restart. Partial or invalid state does not extend that limit. New nodes
require a complete configuration before accepting queries. Replay cannot
regress writer or configuration versions or restore a withdrawn lifetime.

Readiness verifies the approved checkpoint and installed files, then asks each
local daemon the fixed canary `CH TXT ready.internal-dns.`. CoreDNS answers it
before it selects a context, with NOERROR once a snapshot is loaded, so a member
with no contexts is still ready. With an identity key, CoreDNS answers it only
when the query carries that key. dnsdist forwards the canary from loopback, or
from its own listen address, to its backend pool over UDP and TCP, and answers
SERVFAIL when no backend is available. Only NOERROR with the expected TXT answer
is ready. Readiness finishes within 1.5 seconds, inside the example probes'
2-second timeout, and names the daemon that was slow. Any client that reaches a
context can also read the canary; it says only that a CoreDNS process has loaded
state. After a restart the agent waits for its daemons to answer the root SOA
without SERVFAIL, not for the canary, so it does not gate daemons still running
rules from an earlier release. dnsdist adds the canary to retained rules when
it starts; if it cannot place the canary, it starts with startup rules that
refuse every context until the agent installs the approved configuration. An
installation that a live agent owns stays ready while its daemons answer, for
up to one minute from its start. An abandoned activation is not ready, and a
gate is not ready once it has stopped or gated the daemons. Healthy daemons
continue through normal agent stops and restarts. Staging and validation can
continue while approved
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

Set `maxBindings` from measured CoreDNS context memory, snapshot activation
time,
dnsdist rule cost, NATS payload size, and activation recovery time. The example
caps a
shard at 200 contexts. Add another fixed shard before
raising those bounds.

Contexts, access bindings, and publications share a stable shard assignment in
each region. Destination ownership follows the context lifetime, so recreating
an access binding can retain the same destination.

NATS uses TLS with a verified server name, client certificate, and one scoped
identity per process. Control-plane credentials publish serving/publication
subjects and consume ACKs. Delivery is at least once; broker receipt does not
prove that a member has applied the update. Each serving member can consume its
assigned durable subjects and publish only its own hashed ACK subject. Broker
partitions delay updates while the fleet continues serving its applied
configuration.

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

## Publication storage

The default publication store uses the Kubernetes API. Set `publicationStore`
to use JetStream KV. All control-plane replicas must use the same store. See
[publication storage and regional dependencies](../architecture/private-dns/README.md#publication-storage-and-regional-dependencies)
for the placement of zone and regional state.

Regions that share a broker must use the same broker URL. Publication cleanup
waits for every region's bucket to load, even when other regional work can
continue.

### Ownership and retention

Each zone has one compiler owner, protected by a compare-and-swap lease. A
replacement owner advances the writer epoch and republishes unchanged content
so that an unexported update is not stranded. Revisions increase within an
epoch, and serving agents reject older versions. Controllers release zone and
shard leases on shutdown.

JetStream bucket revisions preserve the same compare-and-swap checks as API
resource versions. A new or replacement writer uses an epoch at least as high
as the current Unix time, so replacing a store does not leave the writer below
the agents' retained version limits. Older publications are deleted ten minutes
after a newer publication for that zone is exported, unless a pending outbox
still needs them. Exporters repair missing outboxes after a crash.

A separate region lease selects one controller to run each region's exporter
and acknowledgment refresh. Another controller takes over when that lease is
released or expires. Acknowledgment consumers run on every replica. Zone
compilation and shard planning retain their own leases.

### Switching stores

The `internal-dns-publication-store` ConfigMap records the active store. A
replica configured for a different store refuses to start.

1. Stop every control-plane replica.
2. Set the ConfigMap's `data.store` to `jetstream` or `kubernetes`.
3. Start all replicas with the matching configuration.

Alternatively, set `switchPublicationStore` during a rollout that stops all
replicas before starting replacements, such as a Deployment with the `Recreate`
strategy. The first new replica records the active store.

A first JetStream start without this ConfigMap refuses to run while publication
records remain in the API server. Once writer epochs use clock time, downgrading
to a release that counts epochs from one is unsupported.

## Rollout and recovery

Project APIs select zones by name and report names and readiness. DNS stores
resolved ownership identities in reference ConfigMaps in the service project.
The collector checks source resources before removing those records.

The supported envelope version is `v1alpha6` and checkpoint format is 7. Keep
reference records, transport state, and persistent checkpoints across compatible
upgrades. Deploy
serving agents before controllers that emit per-zone pending state. Enable the
controller change after every serving member supports that contract; a matching
wire version alone does not establish feature compatibility.

Regional CoreDNS members from before the readiness canary refuse it. Until
every regional CoreDNS answers the canary, a regional front treats a REFUSED
canary as a live backend and logs that it did, so fronts and CoreDNS members can
upgrade in either order. This is temporary: once every regional CoreDNS has
the canary, a REFUSED canary should fail front readiness too. A SERVFAIL
canary, which means no backend is available or a backend has no state or the
wrong identity key, is never ready. A node member's dnsdist and CoreDNS run in
one pod and upgrade together, so node readiness requires NOERROR.

The checkpoint retains desired version fences and the activated configuration
separately. Restart can restore the activated configuration during a rejected
expansion or broker outage. A restored checkpoint can serve for at most 15
minutes without a validated bootstrap. Partial or invalid state does not extend
that limit. Bootstrap selects the newest configuration epoch and revision,
rejects conflicting snapshots at that version, and checks the required
publications before activation. Explicit withdrawal invalidates fallback before
installation. Unknown checkpoint or wire formats preserve files and reject
activation. Format conversion requires a tested migration that retains lifetime,
withdrawal, configuration, and publisher fences; never reset those fences to
complete an upgrade.

Snapshot budgets admit established bindings before new allocations. Overflow
reports `CapacityAvailable=False` for the affected binding while healthy
contexts and withdrawals continue. A pending zone fails only queries beneath
its private apex; its names never fall through to public recursion. Bindings,
publication targets, and context status use the same stable shard assignment.

### Control-plane and agent monitoring

The control plane serves `/metrics` on `--metrics-bind-address` (default
`:8080`) and `/healthz` plus `/readyz` on `--health-probe-bind-address`
(default `:8081`). The deprecated `metricsBindAddress` configuration key still
overrides the control-plane metrics flag. Agent metrics are deliberately off by
default for compatibility with pods that already own port 8080; enable their
`/metrics` endpoint explicitly with `--agent-metrics-bind-address`. Set any
listener flag to `0` to disable it and restrict enabled endpoints to the
monitoring network.

`/healthz` fails when a periodic loop has not completed a run in three times
its interval, or ten minutes, whichever is longer. It checks that loops are
running, not that they succeed. `/readyz` waits for the first project
discovery pass and, when admission is enabled, for the webhook server.

The metrics endpoint includes controller-runtime reconciliation metrics and:

- `internal_dns_loop_runs_total{loop,region,shard,result}`: completed loop
  runs. `result` is `success`, `conflict` when every failure in the run lost
  an optimistic-lock race to the other replica, or `error`.
- `internal_dns_loop_consecutive_failures{loop,region,shard}`: errors since
  the loop last succeeded. Conflicts neither raise nor reset it.
- `internal_dns_loop_last_success_timestamp_seconds{loop,region,shard}`: the
  latest successful run.
- `internal_dns_loop_duration_seconds{loop,region}`: run duration. Broker
  reads are counted but not timed, because they wait for messages.

Loops are `project discovery`, `planner`, `ack refresh`, `outbox`, and the
acknowledgment steps `ack read`, `ack apply`, and `ack commit`. A successful
loop can be idle; it does not prove that an endpoint is serving. Combine these
metrics with binding conditions, broker backlog, and DNS queries from a VPC.

`internal_dns_operations_total{operation,result}` and
`internal_dns_operation_last_success_timestamp_seconds{operation}` are
deprecated and will be removed once dashboards move to the loop metrics. They
fold region and shard into `operation` and count conflicts as errors.

Agent processes also expose delivery connection state, aggregate publication
lag, activation failures, and the last successful activation. The series use
only `region`, `shard`, `mode`, and a fixed activation `stage`; they never label
tenant or zone identity. See
[agent monitoring](../enhancements/node-local-private-dns-authority.md#monitoring)
for metric names, interpretation, and required deployment alerts.

### Serving monitoring

Set `metricsAddress` on a CoreDNS render configuration, such as
`"${POD_IP}:9153"`, to serve `/metrics`, `/health` and `/ready` from that
CoreDNS process. The supervisor expands environment references when it starts
the daemon. The host must be an IP address, IPv4 or IPv6, and not an
unspecified one, because a wildcard listener would also accept connections
from the producer network. The listener is off when the field is omitted.
Roll out an image that knows the field before setting it: older agents reject
unknown configuration fields. If the metrics listener cannot start or stops,
CoreDNS logs it and keeps answering queries.

The endpoint includes the standard CoreDNS `coredns_dns_*`, `coredns_forward_*`
and `coredns_cache_*` families, Go and process metrics, and the families
below. `coredns_proxy_*` is not exported, because it labels each series with
an upstream address, and node contexts forward to per-binding addresses.

- `internal_dns_context_queries_total{outcome}`: how the context plugin
  handled each query, such as `served_zone`, `forwarded_public`,
  `refused_unknown_context` or `servfail_zone_pending`. Readiness canaries
  count as `readiness_canary`, or `servfail_canary_bad_identity` when the
  identity key is missing or wrong.
- `internal_dns_context_snapshot_contexts{state}`: ready and pending contexts
  in the active snapshot.
- `internal_dns_context_snapshot_reloads_total{result}`: snapshots `applied`
  or `rejected`. A rejected snapshot leaves the previous one serving.
- `internal_dns_context_snapshot_applied_timestamp_seconds`: when the active
  snapshot was applied.

No metric carries a context, zone, query name or client label.

Set `metricsAddress` and `metricsACLs` on a dnsdist render configuration, such
as `"[${POD_IP}]:8083"` and the scraper's pod CIDR, to serve dnsdist's
Prometheus `/metrics` from that address. dnsdist expands environment
references when it starts, so the dnsdist container needs `POD_IP` from the
downward API. The address must be a specific IP address; if it is not, dnsdist
logs an error, leaves metrics off and keeps answering queries. Only addresses
in `metricsACLs` may connect. The API and dashboard stay closed behind a random
password and API key that dnsdist generates at start and never writes out. The
listener address takes a restart to change; the ACLs apply on reload.

The endpoint includes the dnsdist counters for queries, responses by rcode
(`dnsdist_frontend_*`), rule refusals and SERVFAILs, downstream timeouts and
errors, the `dnsdist_latency_bucket` histogram, per-backend queries, latency
and TCP connections (`dnsdist_server_*`), and dnsdist's own CPU time.

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
