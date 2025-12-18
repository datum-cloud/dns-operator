# Telemetry & Observability

## Overview

Lightweight observability stack for the DNS agent components.

## Components
- **Grafana**: Pre-provisioned Prometheus and Loki data sources.
- **Prometheus**: Single instance scraping dnsdist, dnscollector, and vector
  metrics out of the box.
- **Loki**: Single-binary log storage for dnstap/log forwarding from vector.
- **Namespace**: `dns-monitoring` is created automatically.

## Deploy
Apply the full stack:
```bash
kubectl apply -k config/monitoring
```

Grafana credentials are `admin` / `admin` (stored in
`Secret/grafana-admin`). 

Port-forward to reach the UI:
```bash
kubectl -n dns-monitoring port-forward svc/grafana 3000:80
open http://localhost:3000
```

## Data sources
- Prometheus URL: `${PROMETHEUS_URL}` (default
  `http://prometheus.dns-monitoring.svc:9090`)
- Loki URL: `${LOKI_URL}` (default `http://loki.dns-monitoring.svc:3100`)

If you want to use an existing cluster Prometheus instead of the bundled one,
patch `PROMETHEUS_URL` and remove the `prometheus` entry from
`config/monitoring/kustomization.yaml` before applying.

## Prometheus scraping
The bundled Prometheus scrapes:
- `dnsdist` at `pdns-auth.dns-agent-system.svc:8083` (`/metrics`)
- `dnscollector_exporter` at `pdns-auth.dns-agent-system.svc:9165` (`/metrics`)
- `vector` at `pdns-auth.dns-agent-system.svc:9598` (`/metrics`)
