# agent-powerdns-milo

PowerDNS agent overlay that points the DNS agent at the **Milo core control
plane** (`milo-apiserver`, root scope) as its read and status-write source, for
the drift-detection e2e. Here the DNS CRDs and `DNSRecordSet`s live on the core
CP, not the local kind API, so the agent (`--role=downstream`) must read them
from there.

## What changed vs. `agent-powerdns-federated`

Bases on `../agent-powerdns-federated`, leaving the PowerDNS, Lightningstream,
and RustFS wiring intact. It only retargets the agent's API server:

1. Replaces the `agent-server-config` ConfigMap with a milo-targeted `server-config.yaml`.
2. Mounts a `milo-kubeconfig` Secret at `/milo` on the `pdns-auth` StatefulSet's `manager` container and sets `KUBECONFIG=/milo/kubeconfig` (`deployment-patch.yaml`).
3. Ships that Secret (`milo-kubeconfig-secret.yaml`) — in-cluster `milo-apiserver` + the test-only `test-admin-token`.

`disableNameSuffixHash: true` is kept so the ConfigMap replace keeps the
StatefulSet's existing volume reference valid.

## How the retarget works

> [!IMPORTANT]
> For `--role=downstream`, the read source is set by the `KUBECONFIG` env var,
> **not** a server-config field.

The downstream branch in `cmd/main.go` builds its manager from
`ctrl.GetConfigOrDie()`; it never reads `discovery.*` or
`downstreamResourceManagement.kubeconfigPath` (only the replicator branch does).
`ctrl.GetConfig()` honors `KUBECONFIG` before the in-cluster config, and the
binary registers no `--kubeconfig` flag, so both the reads and the status writes
resolve to the mounted core-CP kubeconfig. The milo values in `server-config.yaml`
are inert under `--role=downstream` but stay correct if run as `--role=replicator`.

## Validate

```sh
kustomize build --load-restrictor LoadRestrictionsNone config/overlays/agent-powerdns-milo
```
