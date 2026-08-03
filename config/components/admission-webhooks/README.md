# Admission webhooks (control-plane registration)

Cross-cluster `MutatingWebhookConfiguration` for DNSRecordSet display
annotations (`display-name` / `display-value`). The webhook **server** runs in
the dns-operator manager; this bundle only registers admission with the
control-plane apiserver.

## Why a separate path

The MWC must version with the image that serves it. Shipping it in
`dns-operator-kustomize` under this path means an old tag (for example
`v0.6.4`, which has no webhook server) cannot carry a registration Flux can
apply. Hand-authoring the MWC in infra decoupled those versions and caused a
production write outage when `failurePolicy: Fail` hit a build with nothing
listening on `:9443` (see [#69](https://github.com/datum-cloud/dns-operator/issues/69)).

`failurePolicy` is `Fail`. With co-versioned registration, a missing webhook
should block DNSRecordSet writes rather than admit without activity
annotations (Ignore has no self-recovery for the audit event that already
fired). Always apply this path from the same OCI tag as the manager
Deployment.

This directory is **not** included in `config/default` or the replicator
overlay. Same-cluster packaging for kind/e2e stays under `config/webhook/`.

## Contents

| Kind | Name | Notes |
| ---- | ---- | ----- |
| `MutatingWebhookConfiguration` | `dns-operator-mutating-webhook-configuration` | `failurePolicy: Fail`; Service ref is `dns-operator-webhook-service` / `datum-dns-system` |

No Service and no kustomize `namespace` transformer: Flux `targetNamespace`
must not rewrite `clientConfig.service.namespace`.

## Flux consumption (infra)

Apply with the **same** `OCIRepository` (`dns-operator-kustomize`) and semver
filter as the manager Deployment:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: core-control-plane-webhooks
spec:
  path: components/admission-webhooks
  sourceRef:
    kind: OCIRepository
    name: dns-operator-kustomize
  kubeConfig:
    secretRef:
      name: milo-configuration-kubeconfig
  dependsOn:
    - name: milo-apiserver
      namespace: datum-system
```

Keep the webhook Service and TLS mounts on the deployment cluster as
environment glue; do not copy the MWC YAML into the infra repo.
