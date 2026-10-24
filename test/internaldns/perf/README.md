# Internal DNS performance tests

Load test the private DNS query path from inside a compute instance, the way a
customer workload uses it. A k6 instance queries its VPC resolver (`fd53::53`)
and measures latency, throughput, and wrong or failed answers for:

- **private**: automatic names of instances in the same VPC
  (`<instance>.datum.internal`), which must resolve.
- **nxdomain**: names that exist only in another project, which must return
  NXDOMAIN.
- **public**: internet names through the recursor. AAAA answers for IPv4-only
  names come from DNS64.
- **propagation** (optional): how long a changed private record takes to be
  served while the resolver is under load.

k6 has no DNS support of its own, so the image is k6 built with
[xk6-dns](https://github.com/grafana/xk6-dns). CI publishes it as
`ghcr.io/datum-cloud/internal-dns-perf` from [Dockerfile](Dockerfile) on pushes
to main (`v0.0.0-main`), tags, and releases.

## Limits

Each generator queries the resolver on its own node, which every other
instance on that node also uses. One staging node resolver failed above about
200 queries per second on 2026-10-22, so `dns.js` refuses to offer more than
`MAX_RATE` (200) queries per second across all its scenarios. Raise it only
when the resolver has been shown to take more, and remember that generators on
the same node add up. Compute does not yet support container resource limits, so
each k6 instance is bounded by its instance type (`datumcloud-d1-standard-2`).

`run.sh` refuses to run unless `datumctl` points at a staging endpoint;
`ALLOW_ANY_ENV=1` overrides this. `down`
deletes only objects labelled `test.datum.net/owner=internal-dns-perf`.

## Prepare a project

Use a project of your own so the load and test instances don't disturb anyone
else. A new staging project needs compute and DNS enabled and an IP quota grant
created inside it before its VPC can get addresses:

```sh
export PROJECT=idns-perf
datumctl create -f - --organization datum-technology <<EOF
apiVersion: resourcemanager.miloapis.com/v1alpha1
kind: Project
metadata:
  name: $PROJECT
spec:
  ownerRef: {kind: Organization, name: datum-technology}
EOF
datumctl create --project $PROJECT -f - <<EOF
apiVersion: quota.miloapis.com/v1alpha1
kind: ResourceGrant
metadata: {name: ipclaims-$PROJECT, namespace: default}
spec:
  consumerRef: {apiGroup: resourcemanager.miloapis.com, kind: Project, name: $PROJECT}
  allowances:
  - resourceType: ipam.miloapis.com/ipclaims
    buckets: [{amount: 10}]
EOF
datumctl services enable compute --project $PROJECT
datumctl services enable dns.networking.miloapis.com --project $PROJECT
datumctl create --project $PROJECT -f - <<EOF
apiVersion: networking.datumapis.com/v1alpha
kind: Network
metadata: {name: default}
spec: {ipFamilies: [IPv6], ipam: {mode: Auto}, mtu: 1440}
EOF
```

The VPC's resolver context appears once the first instance runs in it.

## Run

[run.sh](run.sh) needs only `datumctl` signed in to the environment, with the
compute plugin installed (`datumctl plugin install compute`). It starts k6
with `datumctl compute exec` and reads results back the same way, so it works
with ordinary project access. Exec sessions are not yet released promptly
(datum-cloud/compute#532): after about a dozen execs the project refuses new
ones for 20 minutes or more. For a long series of runs, use autorun instead.

```sh
export PROJECT=idns-perf TARGETS=3 GENERATORS=1
test/internaldns/perf/run.sh up
test/internaldns/perf/run.sh run PRIVATE_RATE=140 NXDOMAIN_NAMES=<name in another project>.datum.internal
test/internaldns/perf/run.sh down
```

`up` creates `idns-perf-target` (idle instances that give the test private
names) and `idns-perf-k6` (the load generators), then waits until both are
ready. `run` starts `dns.js` on every generator at once, prints each summary,
and saves the text, the k6 JSON summary, and a record of every query
(`<generator>.csv.gz`) under `results/`. It then runs `analyze.sh` on them. Each generator queries
the resolver on its own node, so more generators spread the load over more
nodes rather than piling it onto one.

To find where a resolver breaks down, step the private query rate up and look
for the step where latency climbs or answers fail:

```sh
test/internaldns/perf/run.sh run RAMP=50,100,150,200 PUBLIC_RATE=0 STAGE_DURATION=1m NO_THRESHOLDS=1
```

### Propagation under load

In a second terminal, rewrite a TXT record every few seconds while the load
runs. The writer stamps each value with the time it was written; k6 records the
gap between that time and when the new value is first answered.

```sh
INTERVAL=10 DURATION=5m test/internaldns/perf/run.sh propagate
test/internaldns/perf/run.sh run DURATION=5m PROPAGATION_NAME=stamp.perf.internal PRIVATE_RATE=150
```

The result includes clock skew between your machine and the instance, and the
time `datumctl apply` takes, so treat values under a second as noise.

### Running unattended

Set `K6_AUTORUN=1` and the k6 variables as container `env` in
`manifests/loadgen.yaml`. Each instance then runs once at start, prints its
summary to the instance log, and exits with k6's status, so a failed run shows
as a failed instance. The instance restarts and runs again, so delete the
workload once you have the result. Read the summary with:

```sh
datumctl compute logs idns-perf-k6 --project $PROJECT --since=30m
```

## Settings

`dns.js` reads these from the environment:

| Variable | Default | Meaning |
| --- | --- | --- |
| `RESOLVER` | `[fd53::53]:53` | Resolver to query |
| `DURATION` | `2m` | Length of each steady scenario |
| `PRIVATE_NAMES` | set by `run.sh` | Names that must resolve |
| `PRIVATE_TYPE` | `AAAA` | Record type for private names |
| `PRIVATE_RATE` | `150` | Private queries per second per generator |
| `RAMP`, `STAGE_DURATION` | unset, `1m` | Step the private rate through these values instead |
| `NXDOMAIN_NAMES` | unset | Names that must return NXDOMAIN |
| `NXDOMAIN_RATE` | `25` | NXDOMAIN queries per second |
| `PUBLIC_NAMES` | `google.com,cloudflare.com,github.com` | Public names; avoid names answered with a CNAME |
| `PUBLIC_RATE` | `25` | Public queries per second |
| `PROPAGATION_NAME` | unset | TXT name written by `run.sh propagate` |
| `PROPAGATION_POLL_MS` | `100` | Poll interval for that name |
| `P95_MS`, `MIN_SUCCESS` | `50`, `0.999` | Pass thresholds per query kind |
| `NO_THRESHOLDS` | unset | Report without failing the run; the summary says `REPORT ONLY` |
| `MAX_RATE` | `200` | Refuse to offer more queries per second than this |
| `ALLOW_NO_PRIVATE` | unset | Allow a run with no private names |

`run.sh` reads `PROJECT`, `LOCATION` (`us-central-1`), `NETWORK` (`default`),
`TARGETS`, `GENERATORS`, `IMAGE`, `ZONE_DOMAIN`
(`perf.internal`), `INTERVAL`, `DURATION`, and `ALLOW_ANY_ENV`. `run` exits
non-zero when any generator fails a threshold, fails to start, or returns no
summary.

## Reading results

Before the load starts, `dns.js` resolves a name under `.invalid` and stops if
the answer is not recognised as NXDOMAIN, so the NXDOMAIN numbers can be
trusted.

A query counts as expected when private and public names return at least one
address and NXDOMAIN names return NXDOMAIN. Failures are broken down by reason:
`ServerFailure`, `Refused`, `Timeout` (no answer in 5 seconds), `Answered` (an
NXDOMAIN name resolved, which means a cross-project leak), and `Empty` (no
records). A non-zero `dropped iterations` count means the generator could not
keep up with the requested rate, so add generators before trusting higher
rates. xk6-dns reports latency in whole milliseconds.

With `RAMP`, the summary has one line per step, so a step that failed is not
averaged away by the steps that passed. Queries sent while the rate moves
between steps are tagged `ramping` and left out of those lines.

### Query records

Every query is recorded with its finish time, latency, step and outcome
(`ok` or a failure reason). `run.sh analyze [DIR]` (run automatically after
`run`) combines all generators and writes:

- `stages.tsv`: per query kind and step, p50, p90, p99, p99.9 and max latency,
  queries, failures by reason, and the rate actually sent.
- `timeline.tsv`: the same per second the query was sent (finish time minus
  latency), to line up with the resolver's metrics.

Failures spread evenly across a run point at capacity; failures that both
generators see in the same seconds point at a node-wide outage such as a
resolver restart. A timeout is recorded at 5000 ms, so p99 jumps to 5000 once
more than 1% of a step's queries time out.

### Server-side stats

The client sees only answers. To see where time goes and which component
failed, compare a run with the resolver's own metrics for the same seconds:
dnsdist's `dnsdist_latency_bucket`, `dnsdist_frontend_*` rcodes,
`dnsdist_downstream_timeouts`, `dnsdist_server_tcpcurrentconnections` and CPU
time, and CoreDNS's `coredns_dns_request_duration_seconds` and
`internal_dns_context_queries_total`. Both are off unless the deployment sets
`metricsAddress` (see `config/internal-dns/README.md`).

A propagation result where an older value is seen again after a newer one means
different serving replicas answered with different snapshots.
