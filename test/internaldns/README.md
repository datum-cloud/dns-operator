# Internal DNS end-to-end qualification

The current qualification exercises the BIND-only private DNS path:

```text
scoped Compute service account -> project Kubernetes DNS APIs
  -> publication compiler -> durable outbox -> NATS JetStream
  -> two independent regional BIND publication agents
  -> shared regional dnsdist -> shared node BIND/dnsdist -> UDP/TCP client
```

The simulated Compute publisher cannot write platform artifacts or access NATS.
It creates a `DNSRegistration`, uses a registration-pinned
`DNSContributionGrant`, and writes a generation-matched
`DNSRecordContribution` observation. For a managed name it first discovers the
zone and suffix from `DNSResolverContext.status.managedNamespace`; the Compute
resource never chooses a zone.

The fleet is fixed at one node member, one regional dnsdist frontend, and two
regional BIND members. Each BIND member stores its own zone files, validates
them with `named-checkconf -z`, activates isolated context views, and proves the
installed SOA version directly over UDP and TCP. Adding a second context must
leave every serving container identity unchanged.

## Isolated runtime

Every runner requires the suite-owned Colima profile and refuses the default
Docker context. The wrappers export only this socket:

```sh
colima --profile internal-dns-e2e start --activate=false
test/internaldns/run.sh --results test/internaldns/results/bind-e2e-latest.json
```

The socket is `unix://$HOME/.colima/internal-dns-e2e/docker.sock`. Override only
with `INTERNAL_DNS_DOCKER_SOCKET` for the same isolated profile. The runner
creates its own Kind cluster(s), Compose project, kubeconfigs, immutable source
snapshot, runtime configuration, checkpoints, logs, and evidence. Normal
cleanup removes only suite-owned resources. `--keep` or
`INTERNAL_DNS_KEEP=1` retains the environment for diagnosis.

Each run places active BIND configuration, generated zones, checkpoints, and
watchdog leases in a unique VM-local `/tmp/datum-internal-dns-run-*` directory.
This keeps fsync and the five-second fail-closed budget representative of local
serving storage instead of macOS file sharing. Sanitized copies are returned in
the result artifact before that VM-local directory is removed.

The fixture uses a five-second ACK interval and a twenty-second ACK lease so a
single-node Kind API server can project member status without an artificial
write backlog. Member-failure checks still wait beyond the full twenty-second
lease before accepting degraded availability. This suite qualifies ACK
freshness, expiry, and failover semantics; it is not an ACK-throughput or
Kubernetes API capacity benchmark. Production examples use a thirty-second
interval and ninety-second lease.

Prerequisites are Python 3.10+, Docker Compose, Colima, Kind, kubectl, Go,
OpenSSL, and an ARM64 Docker runtime. There is no suite-wide timeout flag; every phase has an
explicit bounded wait. `--results PATH` selects a new evidence file.

## Qualification evidence

The prototype passed all 36 full-path checks and all 37 multi-control-plane
checks before this PR stack was extracted. Re-run both suites on the assembled
stack to qualify its exact commit. Result JSON and sanitized artifacts are
written under `test/internaldns/results/` and remain untracked.

## Full DNS suite

`run.sh` covers:

- overlapping names and addresses in two contexts through the same fixed
  processes over UDP and TCP;
- NXDOMAIN and NODATA cache isolation, plus two private zones in one context;
- managed namespace discovery and scoped Compute publication without a zone
  argument;
- record update and delete, stale registration generation rejection, retired
  grant epoch rejection, health withdrawal, and health recovery;
- original record expiry enforced locally during a NATS outage;
- restart/replay without deadline or writer-fence regression;
- access expiry failing closed while the control plane and broker are absent;
- current publication parity on both regional BIND members; and
- continued queries through the surviving member after either regional member
  and its ACK lease are removed, while readiness reports degraded membership.

The focused serving component check uses static BIND zone files and is separate
from the API publication proof:

```sh
source test/internaldns/colima-env.sh
dev/internal-dns/qualify_proxyv2.py \
  --results test/internaldns/results/bind-proxyv2-latest.json
```

It checks both tiers' PROXYv2 destination selection, overlapping answers,
negative-cache isolation, multiple zones, unknown destination rejection, and
failover with each regional BIND member stopped.

## Multi-control-plane suite

```sh
test/internaldns/run-multi-control-plane.sh \
  --results test/internaldns/results/bind-multi-control-plane-latest.json
```

This suite creates one platform Kind API and two independent source Kind APIs.
Two real control-plane processes watch both source projects and share one
JetStream and serving fleet. Both projects use the same namespace, resolver
context name, zone name, and logical record name while retaining distinct
project, source-cluster, context, zone, registration, grant, and contribution
UIDs.

It additionally checks scoped publisher denial, complete shared-shard snapshots,
one project's update/delete isolation, zone and shard takeover by the surviving
control plane at higher epochs, exact stale-envelope replay rejection, and one
source API blackhole. During that blackhole the unavailable project's original
record and access deadlines expire locally while the healthy project continues
publishing and serving through the same controllers, broker, and fleet. Recovery
cannot resurrect the expired record; a new producer observation is required.

The development admission proxy preserves request bytes and verifies both
backend certificates, but it does not qualify a production load balancer. One
local broker represents one region; the suite does not claim regional broker HA
or a NATS supercluster.

The current serving checkpoint format is version 3. Agents fail closed on
version 2 checkpoints and on checkpoints missing retained binding authority
history. These runs start fresh format 3 agents; they do not qualify an in-place
checkpoint upgrade, long-term checkpoint retention, backup/restore, or NATS
stream migration. The fixture ACK cadence qualifies freshness and failure
semantics under this workload, not fleet-scale ACK or Kubernetes API capacity.

