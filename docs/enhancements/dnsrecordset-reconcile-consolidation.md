# Enhancement: DNSRecordSet Reconcile Consolidation and Orphan Cleanup

**Status**: Implemented on branch feat/rework-dnszone-reconcile-loop
**Author**: Engineering
**Created**: 2026-08-13
**Updated**: 2026-08-13

## Summary

Consolidate DNSRecordSet reconciliation into a single downstream reconciler, add robust finalizer-driven cleanup for DNSRecordSets and DNSZones, and persist namespace-scoped ownership metadata in PowerDNS comments so orphaned records can be traced and cleaned reliably after failures or restarts.

This enhancement is based on changes between feat/support-dnszone-classes and feat/rework-dnszone-reconcile-loop.

## Motivation

### Why this work started

- Multiple controllers were reconciling the same logical DNSRecordSet data paths.
- A single controller was reconciling based on multiple CR event sources and fan-out patterns that amplified reconcile loops.
- DNSRecordSet cleanup was not restart-safe when downstream DNS was temporarily unavailable.

### Problem references

- Issue #59: controller design and reconcile amplification concerns
  - https://github.com/datum-cloud/dns-operator/issues/59
- Issue #58: orphaned recordsets when delete/finalization races failures or restarts
  - https://github.com/datum-cloud/dns-operator/issues/58
- Kubebuilder guidance on avoiding one controller managing multiple CRD responsibilities:
  - https://book.kubebuilder.io/reference/good-practices?utm_source=chatgpt.com#why-should-one-avoid-a-system-design-where-a-single-controller-is-responsible-for-managing-multiple-crds-custom-resource-definitionsfor-example-an-install_all_controllergo

## Goals

- Use one reconciler as the authoritative DNSRecordSet control loop.
- Eliminate duplicate ownership over the same DNSRecordSet programming path.
- Ensure delete flows are idempotent and restart-safe through finalizers.
- Persist namespace-scoped owner identity and observed generation in PDNS for traceability and fast no-op decisions.
- Reduce unnecessary writes to PDNS when desired state is already applied.

## Non-Goals

- Redesigning the DNS API shape.
- Introducing a new CRD.
- Full rework of DNSZone reconcile architecture beyond required cleanup sequencing.

---

## Design Details

### 1. Single DNSRecordSet reconciliation path

The manager now wires DNSRecordSet processing through DNSRecordSetReconciler with DNSHandler, and removes setup of DNSRecordSetPowerDNSReconciler from manager bootstrap.

Effect:
- One reconcile path owns downstream recordset programming.
- Reduced overlap in reconcile ownership and status mutation.

### 2. Finalizer-first delete semantics

DNSRecordSetReconciler now ensures:
- Finalizer is present before steady-state reconciliation.
- On deletion, downstream delete is attempted first.
- Finalizer is removed only after downstream cleanup call succeeds.
- Object is re-fetched before finalizer removal patch for safer state updates.

This closes the gap where records could be deleted in Kubernetes but remain in PDNS if timing or restarts interrupted cleanup.

### 3. Zone deletion sequencing to prevent orphaned records

DNSZoneReconciler deletion path now:
- Lists DNSRecordSets that reference the zone.
- Deletes child recordsets first (triggering their own finalizer cleanup paths).
- Does not requeue on delete errors; orphaned downstream results are expected to be cleared by later reconcile activity and subsequent cleanup passes.
- Once recordsets are gone, deletes zone downstream and removes zone finalizer.

This enforces ordered teardown and avoids deleting the zone while dependent recordsets still exist.

### 4. PDNS ownership and generation metadata

RRSet REPLACE operations now include PDNS comments:
- OWNER: DNSRecordSet namespace:name
- OBSERVED_GENERATION: DNSRecordSet generation

Purpose:
- Trace RRsets back to controlling CR for orphan analysis and cleanup.
- Support quick comparison between applied state and desired generation.

### 5. Generation-aware fast path

During EnsureRecordSet:
- Existing PDNS RRSet comments are inspected.
- If OBSERVED_GENERATION equals current CR generation, replace can be skipped.
- If missing or mismatched, RRSet is replaced and metadata refreshed.

Outcome:
- Fewer redundant writes.
- Faster reconciliation when no spec changes occurred.

### 6. Record-level status propagation

EnsureRecordSet now returns per-owner status entries, and DNSRecordSetReconciler:
- Computes aggregate Programmed condition from owner statuses.
- Persists RecordSets status list deterministically.
- Avoids unnecessary status patching when no effective change.

### 7. Readiness-gated fan-out from DNSZone events

DNSRecordSetReconciler zone watch fan-out now enqueues recordsets only when zone Programmed=True, reducing unnecessary reconcile churn while dependencies are still pending.

---

## Reconcile Flow

### DNSRecordSet steady state

1. Fetch DNSRecordSet.
2. Fetch referenced DNSZone.
3. Verify DNSZone class matches controller client.
4. Ensure DNSRecordSet finalizer exists.
5. Ensure ownerReference to DNSZone exists.
  - DNSRecordSet is a primary CR, so this should be an ownerReference only; do not set controllerReference here because the DNSZone is not a dependent object in this relationship.
6. Set Accepted=True.
7. Call EnsureRecordSet.
8. Update aggregate and per-record Programmed status.

### DNSRecordSet delete path

1. Detect deletion timestamp.
2. Call downstream DeleteRecordSet.
3. Re-fetch resource.
4. Remove finalizer.

### DNSZone delete path

1. Detect deletion timestamp.
2. List referencing DNSRecordSets.
3. Delete those DNSRecordSets and requeue.
4. Once empty, delete zone downstream.
5. Remove zone finalizer.

---

## Files Changed (Implementation Evidence)

- cmd/main.go
- internal/controller/dnsrecordset_downstream_controller.go
- internal/controller/dnszone_downstream_controller.go
- internal/controller/dnszone_downstream_controller_test.go
- internal/dns/client.go
- internal/dns/fake/client.go
- internal/dns/pdns/client.go
- internal/controller/dnsrecordset_powerdns_controller.go

---

## Operational Impact

### Benefits

- Lower risk of orphaned downstream RRsets after controller restarts.
- Cleaner ownership model for DNSRecordSet reconciliation.
- Better supportability via OWNER and OBSERVED_GENERATION metadata in PDNS.
- Reduced unnecessary PDNS writes for already-current generations.

### Risks and Caveats

- OWNER comment stores DNSRecordSet namespace:name. UID was considered unnecessary for the current cleanup flow.
- Deletion-by-comment search depends on PDNS search behavior and consistency.
- More status transitions can increase write volume on status subresource under large-scale updates.

---

## Validation Plan

1. Functional tests
- Create, update, and delete DNSRecordSet resources with multiple owners per type.
- Verify finalizer behavior under normal and failing PDNS conditions.
- Verify zone deletion blocks on child DNSRecordSet cleanup.

2. Failure/restart tests
- Simulate PDNS outage during DNSRecordSet deletion.
- Restart controller while resources are pending deletion.
- Confirm finalizers preserve resources until cleanup is successful.

3. Metadata verification
- Inspect PDNS RRSet comments for OWNER and OBSERVED_GENERATION.
- Confirm reconcile skips replace when observed generation matches current CR generation.

4. Status correctness
- Confirm per-record Programmed conditions are correct.
- Confirm top-level Programmed reflects record-level aggregate state.

---

## Rollout Notes

- No CRD schema migration required.
- Deploy controller image with consolidated reconcile wiring.
- Monitor:
  - reconcile error rate
  - finalizer backlog age
  - PDNS patch/delete error counts
  - orphan/conflict incidents

---

## Follow-Up Work

- Add explicit enhancement links from troubleshooting docs for orphan/conflict handling.
- Consider whether additional disambiguation is needed beyond namespace:name if cross-namespace reuse patterns change.
- Add e2e coverage for restart-safe finalization and generation fast-path behavior.
