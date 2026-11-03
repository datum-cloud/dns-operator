# Staging backend drain validation — 2026-10-29 CDT

The regional backend drain fix passed four real replacement tests. It did **not** eliminate every source of client-visible DNS latency: a separate central guest connectivity interruption occurred after the rollouts.

## Deployment

Both `us-central-1-staging-lab` and `us-east-1-staging-lab` now run the patched image in the `serving` containers of `internal-dns-regional-coredns-0` and `internal-dns-regional-coredns-1`, namespace `ns-1b86cf09-df03-474e-b4f6-d2b82bfe5814`:

`ghcr.io/datum-cloud/internal-dns-serving@sha256:303bcef405614fa1af1cdb9566435ee977f698b3be956bfadf334aecac6b590c`

Image tag: `staging-drain-20261029`. The image extends the previously deployed image (`sha256:dd97c38fc60bcc4a485e0e07e396ba04f1f95b3426e31ad54de705024b695a61`) with amd64 `/internal-dns` and `/usr/local/bin/coredns` binaries built from base commit `741cba912bbfc7e7d871404e70e1e2a524d1e599` plus this drain patch. Agent, node resolver, recursor and singleton frontend images were not changed.

These four Deployments have `kustomize.toolkit.fluxcd.io/reconcile=disabled` to retain the staging image. This is a temporary scoped staging pin, **not** a completed GitOps promotion. Promote the source and resulting image through the normal image channel, verify its rendered backend image, and only then remove these four annotations. Removing them earlier can restore the image without draining. Original Deployment snapshots are saved with the validation evidence for rollback; roll back one backend at a time with a healthy, eligible survivor.

## Procedure

Verified every active regional ready pool included the surviving backend. Installed the image one backend at a time, manually withdrawing each old backend first, waiting five seconds, replacing it, verifying activation and restoring automatic health checks. Then separately replaced each patched backend with **no manual withdrawal**. All four replacement pods passed activation, Kubernetes readiness, actual imageID verification and dnsdist health checks.

Continuous private and public AAAA requests used both UDP and TCP. Regional requests originated in node serving pods, bound to their management addresses and addressed to the context-specific regional frontend VIP. This matters because current node resolvers answer private queries locally; `fd53::53` and `fd53::54` alone would not exercise regional backend replacement. Additional guest probes queried both normal resolver addresses. Regional frontend configuration has no packet cache and uses TCP backend transport. Private responses were checked against the expected target IPv6 address; public responses required NOERROR and an AAAA answer. Probe timeout was three seconds.

Probe window: **2026-10-30 03:32:10–03:38:50 UTC** (Oct 29, 22:32:10–22:38:50 CDT). Probes were stopped intentionally after validation and their temporary binaries/PID files removed; terminal exit 143 records that cleanup, not DNS failures. Results below include all completed samples, including failures.

## Continuous-query results

| Path | Queries | Failures | ≥1 second | p99 (ms) | Maximum (ms) |
|---|---:|---:|---:|---:|---:|
| Central regional | 15,856 | 0 | 0 | 0.997 | 16.104 |
| East regional | 15,783 | 0 | 0 | 0.596 | 5.977 |
| Central guest | 6,370 | 5 | 7 | 1.148 | 3000.992 |
| East guest | 6,368 | 0 | 0 | 0.759 | 9.426 |

## Automatic rollout evidence

Every backend logged entering its five-second drain. dnsdist marked it down after 2.37–2.97 seconds, before the intended listener-close deadline. Each peer continued answering and both members recovered healthy. The table counts direct regional requests initiated between the rollout patch and verified recovery.

| Region/member | Rollout start (UTC) | Recovery (UTC) | Health withdrawal after drain (s) | Queries | Failures | Max (ms) |
|---|---|---|---:|---:|---:|---:|
| central/0 | 03:34:50 | 03:35:06 | 2.389 | 648 | 0 | 12.590 |
| central/1 | 03:35:25 | 03:35:41 | 2.579 | 624 | 0 | 8.451 |
| east/0 | 03:36:31 | 03:36:46 | 2.974 | 593 | 0 | 4.681 |
| east/1 | 03:36:51 | 03:37:06 | 2.367 | 599 | 0 | 4.255 |

Across all four automatic replacement windows: **2,464 direct regional queries, zero failures, no responses ≥1 second, maximum 12.590 ms**. Across the full run: **31,639 regional queries, zero failures, maximum 16.104 ms**. This validates planned one-at-a-time backend replacement with an eligible peer; it does not cover abrupt host failures or singleton frontend replacement.

## Remaining client-network interruption

At **03:37:47.837–03:37:47.844 UTC**, roughly 42 seconds after the last east rollout recovered and over two minutes after the last central rollout recovered, the central guest experienced one simultaneous burst across both resolver addresses:

- Four UDP requests (private/public on each address) timed out after approximately 3,000 ms.
- Three TCP requests succeeded after 1,019–1,027 ms.
- One TCP connection to `fd53::54` failed immediately with `connect: network is unreachable`.

The same guest subsequently recovered. The direct central regional probe had no failures or slow responses during this interval. Neither central node resolver nor the guest restarted. Cilium on the shared host `eris-giune` logged fallback-node-address updates at 03:37:47.451 and 03:37:47.851. Galactic was recompiling service eBPF policies; both local DNS frontends applied configuration changes at approximately 03:37:48. The TCP timing is consistent with a retransmission after packet loss. These are correlations; the exact routing/configuration change responsible has **not** been isolated.

The observed network-unreachable error and simultaneous impact on both client resolver addresses warrant further investigation of the guest-to-node DNS path. The regional drain fix is deployed and verified, but a claim that all multi-second DNS delays are gone would be incorrect.

## Evidence and checks

Raw JSONL queries, timestamped rollout events, old-backend drain logs, frontend health logs, original/final resource snapshots, probe source, deployment scripts and calculated summaries are saved locally under `/tmp/dns-drain-rollout-20261029`. `summary.json` contains per-path statistics and individual rollout windows. The four frontend health transitions can be correlated directly with `central-0-drain.log`, `central-1-drain.log`, `east-0-drain.log`, and `east-1-drain.log`.

Before staging: CoreDNS unit/race drain tests and daemon shutdown tests passed; real dnsdist 2.0.11 integration tested UDP/TCP backend transports with 1,262 queries and no failures. Two unrelated existing shell reload tests could not pass on macOS because the `timeout` executable is absent. No production deployment was changed.
