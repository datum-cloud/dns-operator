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

The Activity Service translates audit logs and Kubernetes events into human-readable activities using CEL-based `ActivityPolicy` resources. We will:

1. Define ActivityPolicy resources for each DNS resource type
2. Update controllers to emit Kubernetes Events for status transitions
3. Deploy policies alongside the DNS operator

### Data Sources

Activities are generated from two sources:

| Source | Use Case | Available Data |
|--------|----------|----------------|
| **Audit Logs** | User actions (create, update, delete) | Full resource spec, user info, response status |
| **Kubernetes Events** | System state changes (Accepted, Programmed, errors) | Event reason, message, object reference (name only) |

### ActivityPolicy Resources

#### DNSZone Policy

```yaml
apiVersion: activity.miloapis.com/v1alpha1
kind: ActivityPolicy
metadata:
  name: dns-dnszone
spec:
  resource:
    apiGroup: dns.networking.miloapis.com
    kind: DNSZone

  # Audit rules have access to full resource - use spec.domainName for display
  auditRules:
    - match: "audit.verb == 'create' && audit.responseStatus.code >= 200 && audit.responseStatus.code < 300"
      summary: "{{ actor }} created zone {{ link(audit.responseObject.spec.domainName, audit.responseObject) }}"

    - match: "audit.verb == 'delete' && audit.responseStatus.code >= 200 && audit.responseStatus.code < 300"
      summary: "{{ actor }} deleted zone {{ audit.requestObject.spec.domainName }}"

    - match: "audit.verb in ['update', 'patch'] && !has(audit.objectRef.subresource)"
      summary: "{{ actor }} updated zone {{ link(audit.responseObject.spec.domainName, audit.responseObject) }}"

  # Event rules - use annotations for structured data
  # Annotation: dns.datumapis.com/domain-name
  eventRules:
    - match: "event.reason == 'Accepted'"
      summary: "Zone {{ event.metadata.annotations['dns.datumapis.com/domain-name'] }} is now active"

    - match: "event.reason == 'Programmed'"
      summary: "Zone {{ event.metadata.annotations['dns.datumapis.com/domain-name'] }} is ready with default SOA and NS records"

    - match: "event.reason == 'DNSZoneInUse'"
      summary: "Zone {{ event.metadata.annotations['dns.datumapis.com/domain-name'] }} conflicts with an existing zone"

    - match: "event.reason == 'Pending'"
      summary: "Zone {{ event.metadata.annotations['dns.datumapis.com/domain-name'] }} is waiting for dependencies"
```

#### DNSRecordSet Policy

Since audit logs contain the full resource, we can access record details directly via CEL. However, constructing human-friendly strings for multiple records is complex in CEL.

**Recommended approach:** Add a `dns.datumapis.com/display-summary` annotation to DNSRecordSet resources that controllers populate with a human-friendly description (e.g., "www.example.com pointing to 192.0.2.10"). This keeps formatting logic in Go where it's easier to handle.

```yaml
apiVersion: activity.miloapis.com/v1alpha1
kind: ActivityPolicy
metadata:
  name: dns-dnsrecordset
spec:
  resource:
    apiGroup: dns.networking.miloapis.com
    kind: DNSRecordSet

  auditRules:
    # Use the display-summary annotation for human-friendly output
    - match: "audit.verb == 'create' && audit.responseStatus.code >= 200 && audit.responseStatus.code < 300"
      summary: "{{ actor }} added {{ audit.responseObject.metadata.annotations['dns.datumapis.com/display-summary'] }}"

    - match: "audit.verb == 'delete' && audit.responseStatus.code >= 200 && audit.responseStatus.code < 300"
      summary: "{{ actor }} removed {{ audit.requestObject.metadata.annotations['dns.datumapis.com/display-summary'] }}"

    - match: "audit.verb in ['update', 'patch'] && !has(audit.objectRef.subresource)"
      summary: "{{ actor }} updated {{ audit.responseObject.metadata.annotations['dns.datumapis.com/display-summary'] }}"

  # Event rules - use annotations for structured data
  # Annotations: dns.datumapis.com/fqdn, dns.datumapis.com/value, dns.datumapis.com/record-type
  eventRules:
    - match: "event.reason == 'Programmed'"
      summary: "{{ event.metadata.annotations['dns.datumapis.com/fqdn'] }} is now resolving to {{ event.metadata.annotations['dns.datumapis.com/value'] }}"

    - match: "event.reason == 'NotOwner'"
      summary: "{{ event.metadata.annotations['dns.datumapis.com/fqdn'] }} pointing to {{ event.metadata.annotations['dns.datumapis.com/value'] }} won't take effect because another record already controls this name"

    - match: "event.reason == 'BackendError'"
      summary: "Failed to apply {{ event.metadata.annotations['dns.datumapis.com/fqdn'] }}"

    - match: "event.reason == 'Pending'"
      summary: "{{ event.metadata.annotations['dns.datumapis.com/fqdn'] }} is waiting for zone to be ready"
```

**Controller responsibility:** The DNS operator must set the `dns.datumapis.com/display-summary` annotation on DNSRecordSet resources with a human-readable description:

```go
// Example annotation values by record type:
// A:     "www.example.com pointing to 192.0.2.10"
// AAAA:  "www.example.com pointing to 2001:db8::1"
// CNAME: "api.example.com as an alias for api.internal.example.com"
// MX:    "mail for example.com using mail.example.com, mail2.example.com"
// TXT:   "TXT record for example.com"
```

#### DNSZoneClass Policy

```yaml
apiVersion: activity.miloapis.com/v1alpha1
kind: ActivityPolicy
metadata:
  name: dns-dnszoneclass
spec:
  resource:
    apiGroup: dns.networking.miloapis.com
    kind: DNSZoneClass

  auditRules:
    - match: "audit.verb == 'create' && audit.responseStatus.code >= 200 && audit.responseStatus.code < 300"
      summary: "{{ actor }} created DNSZoneClass {{ link(audit.responseObject.metadata.name, audit.responseObject) }}"

    - match: "audit.verb == 'delete' && audit.responseStatus.code >= 200 && audit.responseStatus.code < 300"
      summary: "{{ actor }} deleted DNSZoneClass {{ audit.objectRef.name }}"

    - match: "audit.verb in ['update', 'patch'] && !has(audit.objectRef.subresource)"
      summary: "{{ actor }} updated DNSZoneClass {{ link(audit.responseObject.metadata.name, audit.responseObject) }}"
```

#### DNSZoneDiscovery Policy

```yaml
apiVersion: activity.miloapis.com/v1alpha1
kind: ActivityPolicy
metadata:
  name: dns-dnszonediscovery
spec:
  resource:
    apiGroup: dns.networking.miloapis.com
    kind: DNSZoneDiscovery

  auditRules:
    # dnsZoneRef.name is the resource name, but event.message will have the domain name
    - match: "audit.verb == 'create' && audit.responseStatus.code >= 200 && audit.responseStatus.code < 300"
      summary: "{{ actor }} started discovering existing records"

  # Annotation: dns.datumapis.com/domain-name
  eventRules:
    - match: "event.reason == 'Discovered'"
      summary: "Finished discovering existing records for {{ event.metadata.annotations['dns.datumapis.com/domain-name'] }}"

    - match: "event.reason == 'Pending'"
      summary: "Discovery for {{ event.metadata.annotations['dns.datumapis.com/domain-name'] }} is waiting for zone to be ready"
```

### Controller Changes

Controllers must:
1. Set a `dns.datumapis.com/display-summary` annotation on DNSRecordSet with a human-friendly description
2. Emit Kubernetes Events with human-friendly messages for status transitions

**Why annotations?** The Activity Service's CEL templates can access annotations from audit logs. This lets us format complex record information in Go code rather than CEL.

**Why event messages?** The `event.regarding` field only contains an ObjectReference (name, namespace, kind) - not the full resource. The `event.message` field carries the human-friendly context.

**Files to modify:**
- `internal/controller/dnszone_replicator_controller.go`
- `internal/controller/dnsrecordset_replicator_controller.go`
- `internal/controller/dnsrecordset_powerdns_controller.go`
- `internal/controller/dnszonediscovery_controller.go`

**1. Display summary annotation for DNSRecordSet:**

```go
// Set during reconciliation, before any status updates
func buildDisplaySummary(rs *v1alpha1.DNSRecordSet, zoneDomainName string) string {
    record := rs.Spec.Records[0] // Primary record
    fqdn := buildFQDN(record.Name, zoneDomainName)

    switch rs.Spec.RecordType {
    case "A", "AAAA":
        return fmt.Sprintf("%s pointing to %s", fqdn, record.A.Content)
    case "CNAME":
        return fmt.Sprintf("%s as an alias for %s", fqdn, record.CNAME.Content)
    case "MX":
        return fmt.Sprintf("mail for %s", fqdn)
    default:
        return fmt.Sprintf("%s record for %s", rs.Spec.RecordType, fqdn)
    }
}

// Apply to resource
rs.Annotations["dns.datumapis.com/display-summary"] = buildDisplaySummary(rs, zone.Spec.DomainName)
```

**2. Events with annotations:**

Kubernetes Events have ObjectMeta, so we can set annotations on the Event itself. This allows templates to access structured data via `event.metadata.annotations['...']`.

```go
// Helper to emit events with annotations
func (r *Reconciler) emitEvent(obj runtime.Object, eventType, reason, message string, annotations map[string]string) {
    event := &eventsv1.Event{
        ObjectMeta: metav1.ObjectMeta{
            GenerateName: obj.GetName() + "-",
            Namespace:    obj.GetNamespace(),
            Annotations:  annotations,
        },
        Regarding:           corev1.ObjectReference{...},
        Reason:              reason,
        Note:                message,
        Type:                eventType,
        EventTime:           metav1.NowMicro(),
        ReportingController: "dns-operator",
        ReportingInstance:   r.podName,
        Action:              reason,
    }
    r.EventClient.Create(ctx, event)
}

// Zone events
r.emitEvent(zone, corev1.EventTypeNormal, "Accepted", "Zone is now active", map[string]string{
    "dns.datumapis.com/domain-name": zone.Spec.DomainName,
})
r.emitEvent(zone, corev1.EventTypeNormal, "Programmed", "Default records created", map[string]string{
    "dns.datumapis.com/domain-name": zone.Spec.DomainName,
})
r.emitEvent(zone, corev1.EventTypeWarning, "DNSZoneInUse", "Zone conflicts", map[string]string{
    "dns.datumapis.com/domain-name": zone.Spec.DomainName,
})

// RecordSet events - include structured data for template flexibility
r.emitEvent(rs, corev1.EventTypeNormal, "Programmed", "Record is now resolving", map[string]string{
    "dns.datumapis.com/fqdn":        fqdn,                       // "www.example.com"
    "dns.datumapis.com/record-type": string(rs.Spec.RecordType), // "A"
    "dns.datumapis.com/value":       recordValue,                // "192.0.2.10"
    "dns.datumapis.com/zone":        zone.Spec.DomainName,       // "example.com"
})

// Discovery events
r.emitEvent(disc, corev1.EventTypeNormal, "Discovered", "Discovery complete", map[string]string{
    "dns.datumapis.com/domain-name": zone.Spec.DomainName,
})
```

This allows templates to construct summaries flexibly:

```yaml
eventRules:
  - match: "event.reason == 'Programmed'"
    summary: "{{ event.metadata.annotations['dns.datumapis.com/fqdn'] }} is now resolving to {{ event.metadata.annotations['dns.datumapis.com/value'] }}"
```

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
