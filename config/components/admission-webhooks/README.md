# Admission webhooks (control-plane registration)

Cross-cluster admission registration for `DNSRecordSet`: a
`MutatingWebhookConfiguration` for display annotations (`display-name` /
`display-value`) and a `ValidatingWebhookConfiguration` that refuses a write
newly claiming an owner name another record set already holds in the same zone,
either for the same record type or where either record set is a CNAME, since
PowerDNS refuses any other record type beside a CNAME. The webhook **server**
runs in the dns-operator manager; this bundle only registers admission with the
control-plane apiserver.

## Why a separate path

The MWC must version with the image that serves it. Shipping it in
`dns-operator-kustomize` under this path means an old tag (for example
`v0.6.4`, which has no webhook server) cannot carry a registration Flux can
apply. Hand-authoring the MWC in infra decoupled those versions and caused a
production write outage when `failurePolicy: Fail` hit a build with nothing
listening on `:9443` (see [#69](https://github.com/datum-cloud/dns-operator/issues/69)).

The mutating `failurePolicy` is `Fail`. With co-versioned registration, a
missing webhook should block DNSRecordSet writes rather than admit without
activity annotations (Ignore has no self-recovery for the audit event that
already fired). Always apply this path from the same OCI tag as the manager
Deployment.

The validating `failurePolicy` is `Ignore`. The owner name guard moves a
failure that already has a deterministic outcome to the moment it can be
corrected; it decides nothing the agent's first-come election does not decide
again. A guard that blocked every DNS write while the webhook was unreachable
would trade a late condition for the outage in #69, and the per-record
`NotOwner` condition still reports anything that slips through.

The same `ValidatingWebhookConfiguration` registers `/validate-internal-dns` for the five private DNS resources that reference other objects: `DNSZoneAssociation`, `DNSRegistration`, `DNSRecordContribution`, `DNSNamingPolicy` and `DNSResolverAccessBinding`. It checks that each reference names a live object in the same project namespace, that contribution and access binding references still match the pinned UID, that zones are private, and that record owners and types fit the registration. Its `failurePolicy` is `Fail`: nothing downstream re-checks these tenant and lifetime boundaries, so a write must not be admitted without them.

This directory is **not** included in `config/default` or the replicator
overlay. Same-cluster packaging for kind/e2e stays under `config/webhook/`.

## Contents

| Kind | Name | Notes |
| ---- | ---- | ----- |
| `MutatingWebhookConfiguration` | `dns-operator-mutating-webhook-configuration` | `failurePolicy: Fail`; Service ref is `dns-operator-webhook-service` / `datum-dns-system` |
| `ValidatingWebhookConfiguration` | `dns-operator-validating-webhook-configuration` | `vdnsrecordset.kb.io` is `failurePolicy: Ignore`; `vinternaldns.kb.io` is `failurePolicy: Fail`; same Service ref |

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
