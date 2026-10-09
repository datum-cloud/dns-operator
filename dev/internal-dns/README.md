# Internal DNS development environment

This disposable Compose environment models the fixed shared serving topology:

- one node dnsdist and one node BIND;
- one regional dnsdist frontend;
- two independent regional BIND members, each directly hosting the complete
  private zone set in destination-selected context views; and
- one NATS JetStream broker used by the full API-to-DNS qualification.

Adding a resolver context changes destinations, views, and zone files. It does
not create another dnsdist or BIND process. PowerDNS views, network variants,
and per-context source markers are absent.

| Purpose | Address |
| --- | --- |
| NATS from host / containers | `127.0.0.1:14222` / `10.253.0.5:4222` |
| Regional BIND members | `10.253.0.20:5300`, `10.253.0.21:5300` |
| Shared regional dnsdist | `10.253.0.40:53` |
| Static regional destinations | `10.253.0.41`, `10.253.0.42`; reject `10.253.0.43` |
| Shared node BIND | `10.253.0.30:5300` |
| Shared node dnsdist | `10.253.0.50:53` |
| Static node destinations | `10.253.0.51`, `10.253.0.52`; reject `10.253.0.53` |
| Publication verifier peers | `10.253.0.70`, `10.253.0.71` |

The full suite uses the IPv6 node and regional destinations carried in the
internal `DNSResolverBinding`. The runner installs them on the stable dnsdist
VIP namespaces before activation. Regional agents write separate runtime
directories and prove their own BIND member directly with a trusted PROXYv2
destination over UDP and TCP.

Run the focused component qualification in the dedicated Colima profile:

```sh
colima --profile internal-dns-e2e start --activate=false
DOCKER_HOST=unix://$HOME/.colima/internal-dns-e2e/docker.sock \
  dev/internal-dns/qualify_proxyv2.py \
  --results test/internaldns/results/bind-proxyv2-latest.json
```

The component check uses static BIND primary-zone files to isolate dnsdist,
PROXYv2 destination preservation, BIND view/cache isolation, and regional
member failover. The end-to-end suite does not seed those files. Its scoped
Compute simulator creates a registration, grant, and contribution through the
project Kubernetes API; the compiler commits and exports an immutable
publication, and each regional agent renders and proves its own BIND zone copy.

All images use immutable digests. BIND 9.20.29 is an amd64 image and runs under
Docker's arm64 emulation on Apple Silicon.
