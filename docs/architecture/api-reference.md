# API Reference

The operator serves all resources under the API group/version
**`dns.networking.miloapis.com/v1alpha1`**. Generated CRDs live in
[`config/crd/bases`](../../config/crd/bases); runnable samples live in
[`config/samples`](../../config/samples).

| Resource | Scope | Purpose |
|----------|-------|---------|
| [`DNSZoneClass`](#dnszoneclass) | Cluster | Selects backend and nameserver policy |
| [`DNSZone`](#dnszone) | Namespaced | A single authoritative domain |
| [`DNSRecordSet`](#dnsrecordset) | Namespaced | Records of one type, for one or more owner names |
| [`DNSZoneDiscovery`](#dnszonediscovery) | Namespaced | One-shot snapshot of live records |

## DNSZoneClass

Cluster-scoped policy, analogous to a `StorageClass`. Every `DNSZone` references
a class, which determines the backend and how the operator assigns authoritative
nameservers.

| Field | Type | Description |
|-------|------|-------------|
| `spec.controllerName` | string | Backend selector (e.g. `powerdns`). The downstream agent only acts on zones whose class it implements. |
| `spec.nameServerPolicy.mode` | string | Nameserver assignment mode. `Static` is currently supported. |
| `spec.nameServerPolicy.static.servers` | []string | Authoritative nameservers advertised for zones using this class. |
| `spec.defaults.defaultTTL` | int64 | Optional default TTL applied to zones. |
| `status.conditions` | []Condition | `Accepted`, `Programmed`. |

```yaml
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSZoneClass
metadata:
  name: powerdns
spec:
  controllerName: powerdns
  nameServerPolicy:
    mode: Static
    static:
      servers: ["ns1.example.net.", "ns2.example.net."]
```

## DNSZone

Namespaced. Models a single domain.

| Field | Type | Description |
|-------|------|-------------|
| `spec.domainName` | string | Required FQDN (e.g. `example.com`). Immutable once set. |
| `spec.dnsZoneClassName` | string | Reference to a `DNSZoneClass`. |
| `status.nameservers` | []string | Authoritative nameservers, derived from the class policy. |
| `status.recordCount` | int | Number of record sets in the zone. |
| `status.conditions` | []Condition | `Accepted`, `Programmed`. |
| `status.domainRef` | object | Link to the owning `Domain`, exposing its name and assigned nameservers, when present. |

```yaml
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSZone
metadata:
  name: example-com
  namespace: default
spec:
  domainName: example.com
  dnsZoneClassName: powerdns
```

## DNSRecordSet

Namespaced. Models records of one record type within a zone, across one or more
owner names. Each entry in `spec.records` sets one owner name and carries exactly
one typed field matching `spec.recordType`; the backend groups entries that share
an owner name into a single RRset.

| Field | Type | Description |
|-------|------|-------------|
| `spec.dnsZoneRef` | LocalObjectReference | The `DNSZone` in the same namespace. |
| `spec.recordType` | string | One of `A, AAAA, ALIAS, CNAME, TXT, MX, SRV, CAA, NS, SOA, PTR, TLSA, HTTPS, SVCB`. |
| `spec.records[].name` | string | Owner name; `@` for the zone apex. |
| `spec.records[].ttl` | int64 | Optional per-owner TTL. |
| `spec.records[].<type>` | object | Typed record content for the entry. Each typed field's `content` is a single value (e.g. `a.content: "192.0.2.10"`); other types use their own fields (`mx.preference`/`mx.exchange`, `srv.*`, `soa.*`). |
| `status.conditions` | []Condition | `Accepted`, `Programmed`. |
| `status.recordSets[]` | []object | Per-owner-name realized status, including per-record `Programmed`. |

Each entry holds a single value. To give one owner name several addresses, add
one entry per value; the backend groups them into one RRset:

```yaml
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSRecordSet
metadata:
  name: www-a
  namespace: default
spec:
  dnsZoneRef:
    name: example-com
  recordType: A
  records:
    - name: www
      a:
        content: "192.0.2.10"
      ttl: 300
    - name: www
      a:
        content: "192.0.2.11"
      ttl: 300
```

### Multi-owner conflict resolution

When several `DNSRecordSet` resources target the same zone, owner name, and
record type, the agent programs a **single** owner (chosen by oldest creation
timestamp, then name). The agent marks the others `Programmed=False` with reason
`NotOwner`, so conflicting records never silently overwrite each other.

## DNSZoneDiscovery

Namespaced, write-once. Snapshots a zone's live records via DNS queries; performs
no backend writes. Useful for onboarding or verifying an existing domain.

| Field | Type | Description |
|-------|------|-------------|
| `spec.dnsZoneRef` | LocalObjectReference | The `DNSZone` to snapshot. |
| `status.conditions` | []Condition | `Accepted`, `Discovered`. |
| `status.recordSets[]` | []object | Discovered records, grouped by record type. |

## Conditions

Every DNS resource reports status through standard Kubernetes conditions:

| Condition | Meaning |
|-----------|---------|
| `Accepted` | The resource is valid and its dependencies are satisfied. |
| `Programmed` | The backend has realized the desired state. |
| `Discovered` | (`DNSZoneDiscovery` only) The live-record snapshot completed. |

Common reasons include `Pending`, `Programmed`, `DNSZoneInUse` (domain already
claimed by another zone), `NotOwner` (a conflicting record set owns the name),
and `PDNSError` (the backend rejected the change). See
[Replication Model](./replication.md#status-synthesis) for how the operator
synthesizes conditions across clusters.

## DNSOperator (server config)

You configure the operator binary with a `DNSOperator` object passed via
`--server-config`. The API does not serve this object; it configures a running
instance. Sample: [`config/agent/server-config.yaml`](../../config/agent/server-config.yaml).

| Field | Default | Description |
|-------|---------|-------------|
| `discovery.mode` | `single` | `single` (local cluster) or `milo` (discover project control planes). |
| `discovery.internalServiceDiscovery` | `false` | Use internal service addresses when connecting to discovered control planes. |
| `discovery.discoveryKubeconfigPath` | — | Kubeconfig for the platform control plane used for discovery. |
| `discovery.projectKubeconfigPath` | — | Connection template for discovered project control planes. |
| `downstreamResourceManagement.kubeconfigPath` | — | Kubeconfig for the authoritative (downstream) cluster. |
| `downstreamResourceManagement.dnsZoneAccountingNamespace` | `datum-downstream-dnszone-accounting` | Namespace holding the zone ownership ledger. |
| `controllers.dnsRecordSetPowerDNS.maxConcurrentReconciles` | `4` | Concurrent reconciles for the PowerDNS record-set controller. |
| `controllers.dnsRecordSetPowerDNS.rateLimiterBaseDelay` | `1s` | Exponential backoff base delay. |
| `controllers.dnsRecordSetPowerDNS.rateLimiterMaxDelay` | `30s` | Exponential backoff max delay. |

Backend-specific settings live with each backend. For the PowerDNS environment
variables, see [PowerDNS Backend → Configuration](./backends/powerdns.md#configuration).
