# Enhancement: Activity Service Integration

**Status**: Implemented (Phase 1–2; UX polish for operator-readable summaries in issue #62)
**Author**: Engineering
**Created**: 2026-02-12
**Updated**: 2026-07-23

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
- **Operator searchability**: Summaries name DNS hostnames so support can find customer-facing records (e.g. `_dmarc.example.com`)

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
| 10:05:00 | user@example.com deleted www.example.com pointing to 192.0.2.10 |
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
- Record added / updated / deleted
- Record programming failed (async)
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
3. Stamp `display-name` / `display-value` annotations at admission (mutating webhook) so create audits include FQDNs
4. Deploy policies alongside the DNS operator via Kustomize component

### Display annotations

| Annotation | Example | Set by |
|------------|---------|--------|
| `dns.networking.miloapis.com/display-name` | `www.example.com` | Mutating webhook at create/update; replicator safety net |
| `dns.networking.miloapis.com/display-value` | `192.0.2.10` | Same |

Helpers live in `internal/display`. The webhook looks up the parent `DNSZone` to build the FQDN; if the zone is missing, annotations are left unset and policy fallbacks use `spec.records[0].name`.

Admission uses `failurePolicy: Ignore` so a missing webhook server degrades activity FQDNs instead of blocking DNSRecordSet writes. Cross-cluster registration for Datum control planes ships in `config/components/admission-webhooks` (OCI path `components/admission-webhooks`), versioned with the manager image (see that directory's README). Same-cluster kind/e2e packaging stays under `config/webhook/`.

### Data Sources

Activities are generated from two sources:

| Source | Use Case | Available Data |
|--------|----------|----------------|
| **Audit Logs** | User actions (create, update, delete) | Full resource spec, user info, response status |
| **Kubernetes Events** | System failures (ProgrammingFailed) | Event reason, annotations, regarding/related refs |

Chatty `RecordSetProgrammed` / "now live" event rules were removed from the ActivityPolicy to reduce system noise in search (especially `TXT`). Programming failures remain.

### ActivityPolicy Approach

Each DNS resource type has an `ActivityPolicy` with:

- **Audit rules** for CRUD operations — summaries prefer display annotations; update rules read `recordType` from `audit.responseObject.spec` so portal PATCHes that omit `recordType` on the request still match (issue #36). Metadata-only patches (no `requestObject.spec`) do not produce "updated" activities.
- **Event rules** for async controller outcomes — currently programming failures only.

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
      testdata/                 # Fixture notes for policy CEL tests
config/components/
  admission-webhooks/         # Cross-cluster MWC (OCI path for control-plane Flux)
config/webhook/               # Same-cluster Service + MWC (kind / replicator)
internal/display/             # FQDN / display-value helpers
internal/webhook/             # Mutating webhook for display annotations
internal/activitypolicy/      # Policy structure + CEL match tests
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

### Phase 1: ActivityPolicy Resources — done
1. Create `config/milo/activity/` directory structure
2. Implement ActivityPolicy YAML files for each resource type
3. Add Kustomize integration
4. Test policies (structure + CEL fixtures in `internal/activitypolicy`)

### Phase 2: Controller Event Recording — done (failures retained; programmed noise removed)
1. Event recording on zone / recordset replicators
2. Display annotations via mutating webhook + replicator safety net

### Phase 3: Validation
1. Chainsaw e2e: `test/e2e/activity-display/` (create-time FQDN, `_dmarc`, apex, update refresh)
2. Deploy to test cluster and verify human create/update/delete summaries
3. Verify portal PATCH updates no longer DLQ (issue #36)

---

## Alternatives Considered

### Option A: Custom Activity Generation in Controllers

Rather than using audit logs and events, controllers could directly create Activity resources.

**Pros:** More control over activity content
**Cons:** Duplicates Activity Service functionality, harder to maintain, misses API-level changes

### Option B: Webhooks for activity creation

Use admission webhooks to intercept changes and create activities.

**Pros:** Real-time, guaranteed delivery
**Cons:** Adds latency to API calls, single point of failure

We use ActivityPolicy + Events for activity generation. A **mutating** webhook is used only to stamp display annotations at admission (not to create Activity objects), so create audits see the FQDN without redesigning audit plumbing. Registration for control-plane environments must come from the `dns-operator-kustomize` OCI path `components/admission-webhooks` (same semver as the manager image), not a hand-authored MWC in infra.

---

## Open Questions

1. How should we handle bulk operations (e.g., many records updated at once)?
2. Should activities include namespace information for multi-tenant visibility?
3. How should we format multiple record values (e.g., multiple A records for the same name)?
4. Portal activity *detail* views may still emphasize resource ids; summary link text is the FQDN — follow up in the portal if detail prominence is still weak.
