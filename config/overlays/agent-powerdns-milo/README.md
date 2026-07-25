# agent-powerdns-milo

PowerDNS agent overlay that points the DNS agent at the **Milo core control plane** (`milo-apiserver`, root scope) as its DNS read (and status-write) source, for the DNS drift-detection e2e.

In this environment the DNS CRDs and `DNSRecordSet`s live on the Milo core control plane, **not** on the local kind API. The PowerDNS agent (`--role=downstream`) must read `DNSRecordSet`/`DNSZone` objects from the Milo core CP and program PowerDNS from them.

## What changed vs. `agent-powerdns-federated`

This overlay bases on `../agent-powerdns-federated` and leaves the PowerDNS, Lightningstream, and RustFS/S3 (`s3-credentials`) wiring completely intact. It changes only the agent's API target:

1. **Replaces the agent server-config** (`configMapGenerator` `behavior: replace` on `agent-server-config`) with a milo-targeted `server-config.yaml`.
2. **Mounts a `milo-kubeconfig` Secret at `/milo`** on the `manager` container of the `pdns-auth` StatefulSet and sets `KUBECONFIG=/milo/kubeconfig` (`deployment-patch.yaml`).
3. **Ships the `milo-kubeconfig` Secret** (`milo-kubeconfig-secret.yaml`) pointing at the in-cluster `milo-apiserver` endpoint with the static `test-admin-token` and `insecure-skip-tls-verify`.

`disableNameSuffixHash: true` is kept so the StatefulSet's existing `agent-server-config` volume reference stays valid after the replace.

## Which field retargets the read source to the core CP

> [!IMPORTANT]
> For `--role=downstream`, it is **not** a server-config field — it is the `KUBECONFIG` env var.

`cmd/main.go`'s `case "downstream":` branch builds its manager and all three controllers (`DNSZone`, `DNSRecordSet`, `DNSRecordSetPowerDNS`) from `ctrl.GetConfigOrDie()` and `mgr.GetClient()`. It **never** consults `discovery.*` or `downstreamResourceManagement.kubeconfigPath` — those fields are read only by the `case "replicator":` branch (`serverConfig.DownstreamResourceManagement.RestConfig()` / `initializeClusterDiscovery`). So `discovery.mode: single` plus `downstreamResourceManagement.kubeconfigPath` cannot retarget a downstream agent's read source on their own.

The retarget is done by `KUBECONFIG=/milo/kubeconfig` in `deployment-patch.yaml`. controller-runtime's `ctrl.GetConfig()` honors `KUBECONFIG` before falling back to the in-cluster config, and no `--kubeconfig` flag is registered on the binary — so both the reads (DNSRecordSet/DNSZone informers) and the writes (status updates via `mgr.GetClient()`) resolve to the mounted Milo core-CP kubeconfig instead of the in-cluster kind API.

The milo-targeted values in `server-config.yaml` (`discovery` paths + `downstreamResourceManagement.kubeconfigPath` all set to `/milo/kubeconfig`) are kept for consistency and remain correct if this agent is ever run as `--role=replicator`; they are **inert** under `--role=downstream`.

## Assumptions needing live confirmation

- The Milo core CP is reachable at `https://milo-apiserver.milo-system.svc.cluster.local:6443` from the `dns-agent-system`/`dns-control` cluster and serves `dns.networking.miloapis.com/v1alpha1` `DNSRecordSet`/`DNSZone`/`DNSZoneClass` at its **root** endpoint (no aggregation path). Validated live per the task grounding; re-confirm if the endpoint or scope changes.
- `test-admin-token` (system:masters) is present in secret `milo-apiserver-auth-tokens` (`tokens.csv`) in ns `milo-system` and accepted by milo-apiserver's token-auth-file.
- The agent namespace is `dns-agent-system` (inherited from the federated overlay); the `milo-kubeconfig` Secret is created there.
- `KUBECONFIG` retargeting assumes the downstream binary registers no `--kubeconfig` flag (confirmed in `cmd/main.go`). If the code later adds one or stops using `ctrl.GetConfigOrDie()` in the downstream branch, this approach must be revisited (the minimal code alternative would be to have the downstream branch build its cluster from `serverConfig.DownstreamResourceManagement.RestConfig()`).

## Validate

```
kustomize build --load-restrictor LoadRestrictionsNone config/overlays/agent-powerdns-milo
```
