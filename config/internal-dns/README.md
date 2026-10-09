# Internal DNS regional fleet deployment

These manifests show the process boundaries for one shared regional shard.
Replace the example images, addresses, certificates, storage classes, admission
CA injection, and site command wrappers before deployment.

The fixed fleet has three roles:

- A `node` resolver agent owns the shared node dnsdist/BIND pair.
- A `regional-dnsdist` resolver agent owns the shared regional frontend and its
  pool of regional BIND members.
- Two or more `regional-bind` combined agents independently render the complete
  private primary-zone set for their member, validate it with
  `named-checkconf -z`, activate it in isolated BIND context views, prove the
  exact version over UDP and TCP, and publish a leased ACK.

Contexts, access bindings, and zones add destinations, views, and zone files to
these processes. They never create workloads per VPC or zone. Use `node`,
`regional-dnsdist`, and `regional-bind` roles for the multi-member production
layout. The `cluster` and `both` roles are limited to development.

Each member needs a distinct member ID, NATS identity and durable consumers,
checkpoint PVC, configuration PVC, and independent watchdog lease. Treat that
set as a single-writer unit. A rolling update must allocate a new member
identity and storage; do not run two pods against one checkpoint.

## Control-plane and API boundaries

Control-plane replicas can concurrently watch the platform API and source
projects. Publication and shard ownership use resource-version checks and
increasing writer epochs on takeover. Each source project has an authenticated
client and optional request limits. An unavailable source must not prevent ACK
projection or publication for healthy sources.

DNS always consumes `DNSResolverContext` and `DNSResolverAccessBinding` from
the project API. It does not watch a networking API. A trusted integration
principal in `admission.integrationSubjects` creates a context and writes
short-lived regional access intent. Product publishers cannot authorize access.
The context UID, access UID, authorization writer epoch, sequence, and original
deadline are carried into the internal `DNSResolverBinding`.

The control plane allocates a managed namespace when the context requests one.
Compute discovers the context status and publishes through
`DNSRegistration` and `DNSRecordContribution` under a separately issued
`DNSContributionGrant`; it does
not choose a zone for the managed-name path and receives no NATS credentials.

## Serving identity and BIND views

Consumer and regional listener prefixes must be local routes in their dnsdist
network namespaces. Galactic authorizes and translates the consumer path to a
service-side destination. dnsdist preserves that destination in a trusted
PROXYv2 header. Node BIND selects a context view and forwards to its regional
destination. Regional dnsdist selects the same context and load-balances the
healthy BIND members. Every regional BIND member directly hosts the context's
private primary zones.

An owned name without eligible answers carries service-only `TYPE65280`
metadata at that exact name. This private-use record preserves NODATA without
adding a child name or conflicting with a recovered CNAME. Tenant declarations
cannot use that type. Dynamic registrations require TTLs from 1 to 30 seconds;
client caches can retain withdrawn answers for at most that TTL. Static records
retain their explicit RRset TTL, or the default of 300 seconds. Entries in one
static RRset must use the same TTL.

A context without private zones proves recursion with the root SOA over UDP and
TCP. `verifier.publicName` can override this probe. BIND members need a writable
`/tmp` runtime volume even when the image root filesystem is read-only.

Private packet caching stays disabled in dnsdist. Each BIND view has its own
positive and negative cache. `max-ncache-ttl` defaults to five seconds and is
bounded at 60 seconds. Public recursion retains DNSSEC validation; each view
adds validation exceptions only for its private apexes. Unknown or expired
destinations are rejected before cache lookup.

Regional publication proof connects directly to that member's BIND listener,
supplies the exact context destination in a protected PROXYv2 header over both
UDP and TCP, and verifies the installed SOA serial and fingerprint. Restrict
`proxyPeers` to regional dnsdist and the member's local publication agent
network.

## Transactions, health, and expiry

An attached zone that has no active publication gates its context until the
configuration is complete. Pending contexts deny queries, recursion, and cache
answers while retaining the original access authorization. Configuration
completion can restore service without renewing authorization; an actual access
withdrawal still requires a newer source authorization to reactivate.

The agent writes a complete candidate set into its stage directory. Regional
zone files are siblings of `cluster-bind.conf`; validation rewrites file
references to the staged paths and runs `named-checkconf -z`. Installation is
atomic only after every candidate validates. Reload wrappers must wait for the
listener and backend pool before returning so the immediate DNS proof observes
the activated state.

Publication ACKs are leases. A regional frontend and node may keep a context
available while at least one assigned regional member has a fresh current ACK;
the project-facing Ready condition can still report degraded membership. If no
member remains, the frontend gates the context. A regional member that cannot
apply or prove a publication fails closed and stops renewing its watchdog.

Agents preserve original record and access deadlines in their checkpoints.
They remove expired records, rewrite regional zone files, and flush affected
BIND names locally even while NATS or a source API is unavailable. Replay and
restart cannot extend a deadline or regress writer/configuration fences.

The watchdog lease is at most five seconds. It is renewed only after local
expiry enforcement, a complete validated transaction, and required DNS proofs.
The gate must stop or firewall the local private listener without depending on
Kubernetes or NATS. Validation, reload, readiness wait, and proof must fit
inside the watchdog budget; reduce shard size or improve local operations when
they do not.

Mount the transaction stage, active BIND configuration, zone files, checkpoint,
and watchdog lease on low-latency storage local to the serving host. Do not put
the hot transaction or lease path on a desktop file-sharing mount or a remote
filesystem whose fsync latency can consume the watchdog budget. Replication is
provided by the durable publication stream and independent regional members;
each member still has its own crash-safe checkpoint volume.

The transaction stage and active configuration must share one filesystem:
installation uses atomic rename. The examples put the stage at
`/etc/internal-dns/stage` on the configuration PVC and keep checkpoints on the
separate state PVC.

The regional BIND reload helper must reload existing primary zones, for
example with `rndc reload`. `rndc reconfig` only guarantees configuration and
new-zone loading, so it is insufficient when an existing private apex moves to
a newly rendered immutable zone file. The node-tier helper may use the
deployment's normal configuration reload because that BIND tier is recursive.

## Capacity and transport

Set `maxBindings` from measured BIND view memory, validation/reload time,
dnsdist rule cost, NATS payload size, and watchdog timing. The example caps a
shard at 200 contexts and budgets 2 MiB per view. Add another fixed shard before
raising those bounds.

NATS uses TLS with a verified server name, client certificate, and one scoped
identity per process. Control-plane credentials publish serving/publication
subjects and consume ACKs. Each serving member can consume its assigned durable
subjects and publish only its own hashed ACK subject. Broker partitions do not
extend record or authorization deadlines.

`fleet.example.yaml` provides node, regional frontend, and regional BIND JSON
configurations plus one Deployment template. Instantiate the template for each
member and create a second regional BIND member with distinct addresses and
storage. `control-plane.example.yaml` lists both regional members in
`clusterBackends` and `members`.

The supported internal envelope version is `v1alpha3` and the serving checkpoint
format is 4. Unsupported versions fail closed. Deploy matching controller and
fleet versions with fresh internal DNS transport state and checkpoints. Preserve
any existing state for recovery. The deployment requires a coordinated rollout;
it does not support mixed versions or automatic state conversion.
