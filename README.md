## dns-operator

Kubernetes operator for managing DNS zones and records, with a pluggable backend architecture. This repository provides:
- Custom resources to model zones, recordsets, and zone classes
- Controllers for two runtime roles:
  - "downstream" agent that programs a DNS backend (PowerDNS supported)
  - "replicator" that mirrors resources from an upstream cluster to a downstream cluster and synthesizes status
- Kustomize overlays to deploy either role

### CRDs
- **`DNSZoneClass`** (cluster-scoped)
  - `spec.controllerName`: selects backend controller (e.g., "powerdns")
  - `spec.nameServerPolicy`: currently supports `Static` with `servers: []`
  - `spec.defaults.defaultTTL`: optional default TTL for zones

- **`DNSZone`** (namespaced)
  - `spec.domainName`: required zone FQDN (e.g., `example.com`)
  - `spec.dnsZoneClassName`: optional reference to a `DNSZoneClass`
  - `status.nameservers`: authoritative nameservers (derived from class policy)
  - `status.conditions`: `Accepted`, `Programmed`

- **`DNSRecordSet`** (namespaced)
  - `spec.dnsZoneRef`: `LocalObjectReference` to a `DNSZone` in the same namespace
  - `spec.recordType`: one of `A, AAAA, CNAME, TXT, MX, SRV, CAA, NS, SOA, PTR, TLSA, HTTPS, SVCB`
  - `spec.records[]`: owners with typed fields per record type (or `raw` strings). TTL per-owner optional.
  - `status.conditions`: `Accepted`, `Programmed`

### Controllers and Roles

- **Downstream role** (`--role=downstream`)
  - `DNSZoneReconciler`: when `DNSZone.spec.dnsZoneClassName` references a class with `controllerName: powerdns`, ensures the zone exists in PowerDNS and honors static nameserver policy.
  - `DNSRecordSetReconciler`: for PowerDNS-backed zones, applies recordsets to PDNS using an authoritative mode that REPLACEs desired owners and DELETEs extraneous owners of the same type. Requeues while the zone is not ready.

- **Replicator role** (`--role=replicator`)
  - Multicluster manager discovers one or many upstream clusters (single-cluster or Milo discovery) and mirrors `DNSZone`/`DNSRecordSet` into a configured downstream cluster using a mapped-namespace strategy.
  - `DNSZoneReplicator`:
    - Mirrors upstream `spec` into a downstream shadow object
    - Ensures an operator-managed upstream `DNSRecordSet` named `soa` exists (typed SOA targeting `@`) for PowerDNS-backed zones
    - Updates upstream `status`: sets `Accepted=True` and currently treats `Programmed=True` optimistically; fills `status.nameservers` from `DNSZoneClass` when `Static` policy is set
  - `DNSRecordSetReplicator`:
    - Mirrors upstream `spec` into a downstream shadow object
    - Updates upstream `status`: `Accepted` reflects `DNSZone` presence; `Programmed=True` once downstream shadow ensured

### Backends
- **PowerDNS (Authoritative)**
  - Enabled when `DNSZoneClass.spec.controllerName: powerdns`
  - The downstream agent uses environment variables to connect:
    - `PDNS_API_URL` (default `http://127.0.0.1:8081`)
    - `PDNS_API_KEY` or `PDNS_API_KEY_FILE`
  - Recordset translation supports typed fields for all declared RR types and sensible normalization of names and quoting for TXT/targets.

### Deployment Overlays

- `config/overlays/agent-powerdns/`
  - Namespace: `dns-agent-system`
  - Runs the operator with `--role=downstream`
  - Merges a `pdns` sidecar container into the controller Deployment to run PowerDNS alongside the manager
  - Mounts a shared `emptyDir` to exchange an auto-generated API key, and sets `PDNS_API_KEY_FILE` in the manager
  - Provides a `Service` exposing PDNS ports 53/udp, 53/tcp, and 8081/tcp
  - ConfigMap `server-config` wired to `--server-config`

- `config/overlays/replicator/`
  - Namespace: `dns-replicator-system`
  - Runs the operator with `--role=replicator`
  - Requires a Secret `downstream-kubeconfig` containing key `kubeconfig` to target the downstream cluster
  - ConfigMap `server-config` sets discovery mode (defaults to `single`) and points `downstreamResourceManagement.kubeconfigPath` to `/downstream/kubeconfig`

### Quickstart: Agent with embedded PowerDNS
1. Install CRDs and default manifests:
   - `kubectl apply -k config/crd`
   - `kubectl apply -k config/overlays/agent-powerdns`
2. Create a `DNSZoneClass` for PowerDNS with static nameservers, for example:
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
3. Create a `DNSZone` and a `DNSRecordSet`:
```yaml
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSZone
metadata:
  name: example-com
  namespace: default
spec:
  domainName: example.com
  dnsZoneClassName: powerdns
---
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
        content: ["192.0.2.10", "192.0.2.11"]
      ttl: 300
```

### Quickstart: Replicator (upstream → downstream)
1. Create Secret on the replicator namespace containing the downstream kubeconfig (`data.kubeconfig`):
```bash
kubectl -n dns-replicator-system create secret generic downstream-kubeconfig \
  --from-file=kubeconfig=/path/to/downstream/kubeconfig
```
2. Deploy replicator overlay:
```bash
kubectl apply -k config/overlays/replicator
```
3. Create `DNSZoneClass` (cluster-scoped), `DNSZone` and `DNSRecordSet` on the upstream cluster. The replicator will mirror them into the downstream cluster and update upstream `status` conditions.

### Conditions
- `Accepted`: resource is valid and has required dependencies (e.g., `DNSRecordSet` sees its `DNSZone`)
- `Programmed`: desired state is realized (shadow exists downstream; for downstream agent, recordsets applied to backend)

### Configuration CRD (server config)
- `kind: DNSOperator` (internal config consumed by the binary via `--server-config`)
  - `discovery.mode`: `single` or `milo`
  - `downstreamResourceManagement.kubeconfigPath`: path inside the Pod to the downstream kubeconfig

### Development
- Build: `make docker-build` (see `Makefile`)
- Generate code/manifests: `make generate` and `make manifests`
- Local e2e: see `test/e2e/chainsaw-test.yaml` and sample manifests under `config/samples/`
