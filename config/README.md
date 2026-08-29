# Deployment configuration

Kustomize bases, components, and overlays for running the operator. `default`
composes a complete single-cluster deployment; `overlays/` builds the per-role
deployments on top of it; `crd/`, `rbac/`, and `manager/` hold the pieces they
share.

Most of it is ordinary kubebuilder scaffolding. One part is not, and it is the
part that has already caused an outage.

## Admission webhooks are packaged twice

An admission webhook needs a server and a registration. The server is the
manager binary. The registration is a `MutatingWebhookConfiguration` or
`ValidatingWebhookConfiguration` that tells an API server to call it, and this
repository ships **two** of those, for two different API servers.

| Path | Registers with | Composed into | Maintained by |
|---|---|---|---|
| `webhook/` | The same cluster the manager runs in | `default/`, so kind and e2e get it | `make manifests`, from the kubebuilder markers |
| `components/admission-webhooks/` | A separate control-plane API server, reached across clusters | Nothing. Applied on its own, from the published artifact | By hand |

The split exists because the two API servers are not the same machine in a
multi-cluster deployment, and the cross-cluster registration has to version with
the image that serves it. The reasoning is in
[`components/admission-webhooks/README.md`](./components/admission-webhooks/README.md).

> [!IMPORTANT]
> A new webhook must be added to **both** paths.
>
> Adding the kubebuilder marker alone regenerates `webhook/` and nothing else.
> The webhook then works in development, passes review, passes e2e, and is
> absent from every deployment that registers through
> `components/admission-webhooks/`. A guard that never runs where it counts
> fails silently, and nothing in the build says so.

Registration failing closed is not a safety net here. It only fires once
registration exists, which is exactly the case a missing registration is not.
The reverse mistake has its own cost: registration that reaches an API server
ahead of a binary that can serve it blocks writes outright, which is what
https://github.com/datum-cloud/dns-operator/issues/69 records.

For the matching test-side gap, keeping a webhook that is registered but not
served from passing CI, see
https://github.com/datum-cloud/dns-operator/issues/68.
