# DNS MCP server

Deployable bundle for `cmd/dns-mcp`, the read-only MCP server that
lets the Patch assistant diagnose a customer's DNS zones and records.
See [`docs/enhancements/patch-dns-capability.md`](../../../docs/enhancements/patch-dns-capability.md)
for the design this implements.

## Contents

| File | What it is |
| --- | --- |
| `service_account.yaml` | An intentionally unbound `ServiceAccount`. No `Role` or `ClusterRoleBinding` is ever attached to it — see the comments in that file for why. |
| `deployment.yaml` | Runs the `dns-mcp` binary from the operator's own image (`command: [/dns-mcp]`), exposing `/mcp`, `/llms-full.txt`, `/runbooks/*`, and `/healthz` on port 8080. |
| `service.yaml` | A `ClusterIP` Service in front of the Deployment. Cluster-internal only; no Gateway or HTTPRoute fronts it. |

## Deployment

This bundle carries no control-plane configuration. The server refuses
to start against the cluster's own in-cluster API server (see
`checkControlPlaneEndpoint` in `cmd/dns-mcp/main.go`), so the deployer
must patch in a kubeconfig volume naming the Datum control-plane
address and CA, and set `KUBECONFIG` to it.

This bundle is **not** applied by the operator deployment overlays
(`config/agent`, `config/overlays/replicator`). It is deployed by Flux
from the infra repo, alongside the assistant platform's `AgentBinding`
capability document that names this Service's `/mcp`, `/llms-full.txt`,
and `/runbooks/` endpoints.

## Why the server holds no credential of its own

Every read runs as the caller, not as this Deployment's identity. The
server strips every credential off its base kubeconfig and rebuilds a
client per request from the bearer token on that request, pointed at
the project named in the `X-Datum-Project` header — never a tool
argument, since a tool argument is chosen by the model. This keeps the
platform's own RBAC as the single enforcement point and leaves no
ambient authority for a compromised or prompt-injected tool call to
reach beyond what the asker could already see.
