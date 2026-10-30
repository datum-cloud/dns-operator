# Single-use warm foundation experiment

This experiment moves the existing three-cluster foundation preparation before a
job acquires its executor. It provides a verifiable handoff between that preparation
and DNS deployment. It does **not** implement a pool broker, clone running clusters,
register runners, change CI routing, or bake running processes into an OCI image.
The ordinary E2E workflow remains unchanged.

## Execution model

Use one disposable Linux VM (or similarly isolated Docker-capable executor) per
future CI job. That executor owns its Docker daemon and the three fixed cluster
names. Prepare the clusters and their shared add-ons, leave them running, then
claim them once when the job arrives. Destroy the executor after the job, including
a failed job or failed claim; do not return it to the pool. Replenish by preparing a
new clean executor. The claim receipt is concurrency protection between cooperating
clients, not a security boundary against a job with Kubernetes admin access.

The local `infra` ARC configuration currently has an unprivileged `datum-linux`
runner pod with no Docker daemon/sidecar and `minRunners: 0`. Changing only its
runner image cannot host the existing kind topology. This prototype therefore
needs a dedicated Docker-capable executor; integrating a VM allocator or an
appropriately isolated runner configuration is a separate prerequisite. It does
not require changing the general ARC runner pool to perform the experiment.

Run preparation and consumption **on the same executor** (a fresh checkout of the
same foundation inputs is fine). Remote `DOCKER_HOST` alone is insufficient:
exported API endpoints must also be reachable by kubectl. Do not run this against
a shared developer Docker daemon or any production Kubernetes context. The script
uses named kind clusters and explicit generated kubeconfigs, never the user's
current Kubernetes context.

## What is prepared

- Dual-stack `dns-control`, `dns-upstream`, and `dns-edge` kind clusters.
- Flux, cert-manager/CSI, Kyverno, and Envoy Gateway installed through the exact
  pinned `test-infra` revision used by ordinary CI.
- A ConfigMap receipt in the control cluster's `kube-system` namespace, containing
  a schema version, foundation fingerprint, cluster identities, expiry, and state.

DNS images, DNS CRDs, RustFS, secrets, and test fixtures are deliberately absent.
The current PR is built/deployed after claim. This preserves the existing DNS
qualification semantics and measures foundation savings separately from image
builds and deployments.

`prepare` refuses any pre-existing named cluster and verifies the downloaded
`test-infra` checkout matches the immutable pin without tracked modifications.
It records a receipt only after all three APIs/nodes, Flux/Kyverno controllers,
cert-manager/Envoy Helm releases, and gateways pass readiness checks. Nodes must
be dual-stack. DNS namespaces/CRDs cause preparation or claim to fail.

The fingerprint conservatively includes the full Taskfile, kind-preparation
script, this handoff script, and kind version. A Taskfile change therefore
invalidates the foundation even if it affects only a DNS task. This is intentional
for the first experiment; a later change could extract a smaller immutable
foundation definition once this boundary proves useful. Use default task variables
for this experiment; arbitrary bootstrap overrides are not supported.

`claim` verifies the fingerprint, prepared state, and one-hour default lifetime,
then atomically replaces the receipt using its Kubernetes `resourceVersion`.
Concurrent claimers cannot both succeed. It subsequently verifies health,
cleanliness, and unchanged cluster UIDs and exports `dev/kubeconfig`. An unhealthy
or replaced cluster consumes the claim and fails closed. There is no release/reset
operation. The executor's owner handles destruction after collecting diagnostics.
A preparation failure also leaves evidence intact for explicit cleanup.

## Test after PR #432 CI finishes

Use the same executor capacity and dependency versions for both measurements.
Do not treat a second run on a cached machine as evidence solely of pool savings.
Start on a fresh disposable executor with the usual Task/kind/kubectl/Docker and
Go prerequisites. The following commands run from the repository root.

Preparation happens before the simulated job arrives:

```sh
export TASK_X_REMOTE_TASKFILES=1
python3 hack/time-ci-phase.py foundation-prepare \
  task --yes env:foundation:prepare-warm
```

The executor can now sit idle (for less than the receipt TTL). For a deliberately
longer test, invoke `python3 hack/ci/warm-foundation.py prepare --ttl-seconds 7200`
instead of the preparation task on a fresh executor. Expired receipts are rejected;
rebuild rather than manually modifying their expiry.

After assigning the prepared executor to a single job:

```sh
python3 hack/time-ci-phase.py foundation-claim task --yes env:foundation:claim-warm
python3 hack/time-ci-phase.py shared-prerequisites task --yes env:shared-prerequisites
python3 hack/time-ci-phase.py parallel-lanes python3 hack/run-dns-lanes.py --include-harness
```

The experiment workflow `.github/workflows/warm-foundation-experiment.yml` runs
on this experiment branch. It uses a fresh GitHub-hosted runner, times preparation
and claim separately, verifies a second claim is rejected, then runs the same
shared prerequisites and parallel DNS lanes as PR #432. Harness tests run in a
separate job. Preparation and consumption occur in one workflow: this validates
the handoff and qualification, but does not measure real pool assignment latency,
long idle residence, or snapshot restore. Keep the preparation cost separate from
the simulated consuming job's critical path.

Run these sequentially and stop on any error (use `set -e` in a script). Retain
`dev/ci-timings.jsonl` and qualification evidence. After evidence collection,
`task --yes env:down` removes the test clusters, then destroy the dedicated executor.
If preparation/claim fails, collect diagnostics and destroy that executor rather
than continuing or retrying it as an available warm environment.

Compare the current-head cold foundation timing from PR #432 with the successful
`foundation-claim` timing. Their difference is the measured *foundation* saving;
subtract any future broker assignment/connection latency as well. The latest passing parallel CI run 40990741956
measured a 370.802s foundation duration is a reference, not a promised saving. Report deployment
and test durations independently. Preparation remains real compute cost shifted
out of the job's critical path; include idle-pool cost when judging the tradeoff.
Run cold-bootstrap qualification separately so dependency installation failures
continue to be tested.

## Local contract checks

```sh
python3 -m unittest discover -s hack/ci -p 'test_warm_foundation.py'
```

These exercise invalid/stale receipts, reused or replaced environments,
resourceVersion conflict behavior, dirty cluster rejection, dual-stack readiness,
and kubeconfig export. They do not establish real kind/Flux compatibility. The
live experiment above is required before wiring this into CI or a pool allocator.
