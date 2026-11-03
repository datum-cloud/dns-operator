# Node-local Private DNS authority

## Decision

This capability is implemented and disabled by default. It is separate from
the proposal for independent regional control planes.

Node CoreDNS can opt in to serving validated Private DNS publications directly.
Regional control planes still own compilation and publication, and regional
query-serving members remain available during migration. The opt-in removes the
regional query hop; it does not move publication ownership to nodes.

Set the node agent to `mode: "combined"` and
`nodeAuthoritative: true` while retaining `render.role: "node"`. No other
combination accepts the option. Resolver mode keeps its existing forwarding
behavior. `publicationDNSProbe.server` must name that node's protected CoreDNS
listener; the resolver probe continues to test the consumer path.

## Delivery and activation

Every node already consumes the shard's serving snapshot, publication chunks,
and manifests. An authoritative node uses the existing safety path:

1. Verify the immutable manifest and its complete publication plan.
2. Persist writer, contribution, binding, and tombstone fences before changing
   serving state.
3. Render the publication into the node context at its consumer destination.
4. Validate and atomically install the complete dnsdist/CoreDNS snapshot.
5. Verify the publication directly and verify the consumer path over UDP and
   TCP before reporting resolver access ready.

Publication activation is local. Node publication acknowledgments are not sent
to the regional publication readiness aggregator, so an ordinary record update
does not wait for every node. The existing serving acknowledgment continues to
report each node's applied access path. Regional publication status alone does
not prove that every node has applied the latest records.

Unknown destinations remain REFUSED. A known context whose serving snapshot is
pending returns SERVFAIL. A missing, incomplete, withdrawn, or below-fence
private zone is installed as pending and returns SERVFAIL beneath that apex; it
never falls through to public recursion. Public names use the context's separate
cache and `render.nodeCoreDNS.publicUpstreams`. Configure and verify those
upstreams before enabling node authority; queries no longer pass through
regional dnsdist or CoreDNS. Private packet caching remains disabled.

### Bootstrap validation

Before activation, the agent loads a consistent serving snapshot and its
required publications. If retained snapshots from several owners exist, it
selects the highest configuration epoch, then revision. It does not combine old
bindings with the current assignment. Different snapshots at the same winning
version are rejected.

The agent verifies manifest identity, complete chunks, and the minimum
publication version required by each attached zone. Partial delivery cannot
activate new authority or report access ready. A configured pending zone remains
pending; bootstrap does not make it eligible to answer.

## Outage and stale-serving policy

This option keeps the existing serving policy rather than adding a new expiry:

- A running node retains its last accepted snapshot during a transient broker
  or control-plane outage.
- After process restart, the saved snapshot may restore immediately, but the
  agent must complete a validated bootstrap within 15 minutes or it gates
  serving. Receiving partial or invalid state does not reset the deadline.
- A delivered tombstone or access withdrawal advances the durable fence before
  activation. If the withdrawal cannot be installed safely, serving gates
  rather than retaining withdrawn data.
- Replay below a durable writer, contribution, binding, or tombstone fence is
  ignored. A new node remains fail-closed until it bootstraps a serving snapshot
  and every required publication at or above its fence.

The policy intentionally does not make DNS records expire by wall-clock time.
Withdrawal latency during a delivery outage is therefore bounded by restoring
delivery, not by an invented record TTL. Client caching remains bounded by each
record's existing DNS TTL after the node applies the withdrawal.

## Monitoring

Enable the agent endpoint explicitly with `--agent-metrics-bind-address`; it is
off by default so a capable image does not claim a port in existing pods. The
endpoint exports only region, shard, mode, and fixed-stage labels:

- `internal_dns_agent_delivery_connected`
- `internal_dns_agent_publication_lagging_zones`
- `internal_dns_agent_publication_revision_lag`
- `internal_dns_agent_activation_failures_total`
- `internal_dns_agent_last_activation_success_timestamp_seconds`

`publication_revision_lag` compares revisions only inside one writer epoch.
`publication_lagging_zones` is the authoritative signal for missing
publications, epoch changes, tombstones, and failed verification. Neither
metric labels a tenant, binding, zone, member, or destination.

Deployment monitoring should alert when delivery is disconnected for the
regional recovery budget, when lagging zones remain nonzero after delivery has
recovered, or when activation failures increase. Alert routing, scrape policy,
and thresholds belong in the deployment repository; they should be installed
before the first node opts in.

## Migration and rollback

1. Ship the capable image to node agents with the option off. Keep regional
   dnsdist and CoreDNS unchanged.
2. Enable agent metrics and alerts, then opt in one replacement node with its
   own member identity and storage.
3. Verify bootstrap with retained snapshots from previous owners, restart during
   a delivery outage, record updates, tombstones, access withdrawal, public
   recursion, tenant isolation, UDP, and TCP behavior. Check that incomplete
   bootstrap cannot extend restored serving beyond 15 minutes.
4. Expand by node and region. Do not require all nodes to acknowledge one
   publication before continuing.
5. Only after the node fleet is validated may deployment configuration remove
   the node-to-regional query route. Regional publication ownership remains.
6. Roll back by replacing a node with `mode: "resolver"` and
   `nodeAuthoritative: false`. A node-authoritative active checkpoint is not
   restored as a forwarder; the resolver must activate a freshly rendered
   forwarding snapshot. Keep regional serving until that succeeds.

Removing regional query-serving resources is a later deployment change, not
part of this capability. It requires validated node capacity and a rollback
target in every region.
