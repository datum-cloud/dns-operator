# Enhancement: Activity Service Integration

**Status**: Proposed
**Author**: Engineering
**Created**: 2026-02-12

## Summary

Integrate the Activity Service with the DNS Operator to provide human-readable activity timelines for consumers and service providers.

## Motivation

Currently, users and support staff must inspect Kubernetes resources directly to understand what's happening with their DNS infrastructure. This requires technical knowledge and doesn't provide a clear timeline of events.

By integrating with the Activity Service, we provide a transparent, unified view of all DNS activity that both consumers and service providers can use to understand system behavior.

## Goals

- **Transparency**: Consumers and service providers see the same activity timeline
- **Clarity**: Non-technical users can understand what's happening to their DNS resources
- **Visibility**: All meaningful state transitions are captured and surfaced
- **No internal details exposed**: Backend technology and cluster topology remain hidden

## Non-Goals

- Real-time alerting (handled by separate monitoring systems)
- Replacing Kubernetes Events (activities are derived from events, not replacing them)

---

## Proposed Activity Timeline

Both consumers and service providers see the same activity timeline, providing full transparency into what's happening with DNS infrastructure.

Activities should use human-friendly display names (e.g., "example.com" not "example-com") and include relevant DNS information like IP addresses and record values.

### Example Timeline

| Timestamp | Activity |
|-----------|----------|
| 10:00:00 | user@example.com created zone example.com |
| 10:00:01 | Zone example.com is waiting for dependencies |
| 10:00:02 | Zone example.com is now active |
| 10:00:05 | Zone example.com is ready with default SOA and NS records |
| 10:01:00 | user@example.com added www.example.com pointing to 192.0.2.10 |
| 10:01:02 | www.example.com is now resolving to 192.0.2.10 |
| 10:02:00 | user@example.com added api.example.com as an alias for api.internal.example.com |
| 10:02:02 | api.example.com is now resolving to api.internal.example.com |
| 10:03:00 | user@example.com configured mail for example.com to use mail.example.com and mail2.example.com |
| 10:04:00 | user@example.com added www.example.com pointing to 192.0.2.20 |
| 10:04:01 | www.example.com pointing to 192.0.2.20 won't take effect because another record already controls this name |
| 10:05:00 | user@example.com removed www.example.com pointing to 192.0.2.10 |
| 10:05:01 | www.example.com is now resolving to 192.0.2.20 |

### Error Scenario

| Timestamp | Activity |
|-----------|----------|
| 10:00:00 | user@example.com added www.example.com pointing to 192.0.2.10 |
| 10:00:01 | www.example.com is waiting for zone to be ready |
| 10:00:05 | Failed to apply www.example.com |
| 10:00:10 | www.example.com is now resolving to 192.0.2.10 |

### Activity Categories

**Zone Lifecycle:**
- Zone created / updated / deleted
- Zone is now active
- Zone is ready with default records
- Zone conflicts with existing zone
- Zone is waiting for dependencies

**Record Lifecycle:**
- Record added / updated / removed
- Record is now resolving
- Record won't take effect (another record controls this name)
- Failed to apply record
- Record is waiting for zone

**Discovery:**
- Discovery started
- Discovery completed
- Discovery is waiting for zone

---

## Design Details

### Overview

The Activity Service translates audit logs and Kubernetes events into human-readable activities using CEL-based `ActivityPolicy` resources. We:

1. Define ActivityPolicy resources for each DNS resource type
2. Emit `events.k8s.io/v1` Events from controllers for async status transitions
3. Deploy policies alongside the DNS operator via Kustomize component

### Data Sources

Activities are generated from two sources:

| Source | Use Case | Available Data |
|--------|----------|----------------|
| **Audit Logs** | User actions (create, update, delete) | Full resource spec, user info, response status |
| **Kubernetes Events** | System state changes (Programmed, errors) | Event reason, annotations, regarding/related refs |

### ActivityPolicy Approach

Each DNS resource type has an `ActivityPolicy` with:

- **Audit rules** for CRUD operations — these have access to the full resource via `audit.responseObject`, so summaries can reference `spec.domainName` and display annotations directly.
- **Event rules** for async controller outcomes — these use annotations on the `events.k8s.io/v1` Event object (prefixed `dns.networking.miloapis.com/`) for structured data like domain names, record types, and failure reasons.

RecordSet event rules use `event.related` (the parent DNSZone) as the link target so activity timeline entries navigate to the zone detail page. Zone event rules use `event.regarding` (the zone itself).

The actual policy manifests live in `config/milo/activity/policies/`.

### Controller Event Emission

Controllers emit `events.k8s.io/v1` Events directly via a typed client (`EventsV1().Events(ns).Create`) rather than the legacy `record.EventRecorder`. This gives full control over `regarding`, `related`, and `annotations` fields. Event emission is best-effort — failures are logged and swallowed.

### Directory Structure

```
config/milo/
  kustomization.yaml          # Component for control plane installation
  activity/
    kustomization.yaml
    policies/
      kustomization.yaml
      dnszone-policy.yaml
      dnsrecordset-policy.yaml
```

### Kustomize Integration

The `config/milo` directory is a Kustomize component that can be installed into project control planes. Include it in your control plane kustomization:

```yaml
components:
  - ../milo
```

---

## Query Examples

### Recent zone activities

```yaml
apiVersion: activity.miloapis.com/v1alpha1
kind: ActivityQuery
metadata:
  name: my-zone-activities
spec:
  startTime: "now-24h"
  resourceKind: DNSZone
  filter: "spec.resource.name == 'example-com'"
  limit: 50
```

### All DNS record errors

```yaml
apiVersion: activity.miloapis.com/v1alpha1
kind: ActivityQuery
metadata:
  name: dns-errors
spec:
  startTime: "now-1h"
  resourceKind: DNSRecordSet
  filter: "spec.summary.contains('Failed')"
  limit: 50
```

---

## Implementation Plan

### Phase 1: ActivityPolicy Resources
1. Create `config/milo/activity/` directory structure
2. Implement ActivityPolicy YAML files for each resource type
3. Add Kustomize integration
4. Test policies using PolicyPreview API

### Phase 2: Controller Event Recording
1. Add event recording to zone replicator controller
2. Add event recording to recordset replicator controller
3. Add event recording to recordset backend controller
4. Add event recording to discovery controller

### Phase 3: Validation
1. Deploy to test cluster
2. Create DNS resources and verify activities appear
3. Test error scenarios (conflicts, backend errors)
4. Verify activity summaries are clear and actionable

---

## Alternatives Considered

### Option A: Custom Activity Generation in Controllers

Rather than using audit logs and events, controllers could directly create Activity resources.

**Pros:** More control over activity content
**Cons:** Duplicates Activity Service functionality, harder to maintain, misses API-level changes

### Option B: Webhooks

Use admission webhooks to intercept changes and create activities.

**Pros:** Real-time, guaranteed delivery
**Cons:** Adds latency to API calls, single point of failure

The proposed approach (ActivityPolicy + Events) was chosen because:
- Decouples activity generation from DNS controller logic
- Leverages existing Activity Service infrastructure
- Policy-based rules are easier to update without code changes
- Works with audit logs to capture all API changes

---

## Open Questions

1. How should we handle bulk operations (e.g., many records updated at once)?
2. Should activities include namespace information for multi-tenant visibility?
3. How should we format multiple record values (e.g., multiple A records for the same name)?
