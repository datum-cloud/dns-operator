# CI foundation and parallel DNS lanes

The shared foundation consists of three dual-stack kind clusters (`dns-control`,
`dns-upstream`, and `dns-edge`) with Flux, cert-manager, Kyverno, and Envoy Gateway.
CI prepares the foundation, installs shared CRDs/tools, then runs two lanes:

```text
foundation → shared prerequisites ┬ private deployment → private qualification
                                 └ public deployment  → Chainsaw
harness tests (independent GitHub job)
```

Run the same deployment/test lanes locally with:

```sh
task --yes env:foundation-up
task --yes env:shared-prerequisites
python3 hack/run-dns-lanes.py --include-harness
```

`task e2e` runs this sequence. Its optional third lane runs harness tests locally;
CI runs them in a separate job with no dependency on E2E, so they remain CI
checks without serializing environment startup. The runner waits for every lane,
even on failure, before diagnostics and teardown. Tests in a failed deployment's
lane are skipped; the other lane completes. Cancellation signals reach each
phase's process group.

## Shared-cluster interference audit

- Both deployers normally install DNS CRDs, and public Kustomize bundles include
  them again. `env:shared-prerequisites` installs them once on all three clusters,
  installs upstream networking CRDs, and waits for CRDs to be established.
  The lane runner sets `DNS_E2E_SHARED_PREREQUISITES=1`: deployment tasks skip CRD
  writes and public CI overlays exclude CRDs. Do not set this flag without first
  running the prerequisites task.
- Kustomize, controller-gen, and Chainsaw are installed before the split. Lane
  tasks skip installer execution, avoiding concurrent writes to tool symlinks.
- Public replicator admission includes private resource types. Its CI-only
  overlay excludes the private fixture namespace `project-e2e` from public
  webhooks, preventing a fail-closed public webhook startup window from blocking
  private tests. Private qualification retains its dedicated `internal-dns-e2e`
  admission webhook. Public test namespaces still use the public webhooks.
- Private reset only deletes owned `project-e2e`/`internal-dns-system` fixtures;
  failover scenarios target private controllers, NATS, and serving pods. Public
  controllers ignore private zones; classes and workload namespaces are distinct.
- Private network fixtures add routes/NAT exceptions for `fd53::/64` and
  `fd54::/64`, and use NodePort 30443. Public DNS uses its own Services and RustFS's
  dynamically allocated NodePort. The node networking is still shared.
- Private/public image tags, generated files, and kubeconfig paths are distinct.
  Builds read the same source and share Docker/CPU/memory; no lane regenerates Go
  source. Go caches support concurrent use. Resource contention and Kubernetes
  interaction remain subjects of the live CI comparison, not proven speedups.

## Deployment compatibility

`env:internal-dns:up` and `env:stack-up` remain standalone convenience commands.
Without the lane runner's environment flag, they still install CRDs normally.
The public image is built once per Task invocation and loaded into all clusters.
Control and edge create S3 credentials before applying StatefulSets and wait for
the initial rollout without forcing a restart. On existing environments, changed
S3 credentials require restarting existing pods to refresh environment variables.
`env:down` deletes all three clusters. Existing IPv4-only clusters must be recreated
before private DNS deployment.

## Timings and evidence

The `e2e-phase-timings` artifact contains `ci-timings.jsonl` and per-lane logs,
JSONL timings, and summaries under `ci-lanes/`. Each phase reports seconds and exit
status; each lane has a wall-time record, plus an outer `parallel-lanes` duration.
Do not add lane/phase totals to the outer duration: they overlap. Per-lane files
avoid concurrent summary/JSON writes and are merged only after both lanes finish.
The independent job publishes `harness-phase-timings`. An abruptly cancelled
runner may not finish writing a record.

Compare the outer parallel duration with the prior sequential sum of deployment
and qualification/test phases, and compare total E2E job time separately (runner
startup varies). Successful cold foundation duration remains an upper bound on
warm-pool savings, minus future acquisition/readiness overhead. Shared CRDs and
DNS deployment are revision-specific and remain outside the prepared foundation.

Run orchestration and timing tests without a cluster:

```sh
python3 -m unittest discover -s hack -p 'test_*ci*.py'
```
