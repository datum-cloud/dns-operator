// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	sharedutil "go.miloapis.com/dns-operator/internal/dns/util"
)

// The identifiers below re-export go.miloapis.com/dns-operator/internal/dns/util
// so the CLI's existing call sites keep compiling unchanged after the pure
// zone/record/delegation/ownership logic moved to a package the dns-mcp
// server can also import. internal/dns/util is the one definition; this file
// is only wiring.

const (
	CondAccepted   = sharedutil.CondAccepted
	CondProgrammed = sharedutil.CondProgrammed
	CondDiscovered = sharedutil.CondDiscovered

	ReasonAccepted                  = sharedutil.ReasonAccepted
	ReasonPending                   = sharedutil.ReasonPending
	ReasonInvalidDNSRecordSet       = sharedutil.ReasonInvalidDNSRecordSet
	ReasonProgrammed                = sharedutil.ReasonProgrammed
	ReasonDiscovered                = sharedutil.ReasonDiscovered
	ReasonDNSZoneInUse              = sharedutil.ReasonDNSZoneInUse
	ReasonNotOwner                  = sharedutil.ReasonNotOwner
	ReasonPDNSError                 = sharedutil.ReasonPDNSError
	ReasonConflict                  = sharedutil.ReasonConflict
	ReasonPendingDomainVerification = sharedutil.ReasonPendingDomainVerification

	StatusOK         = sharedutil.StatusOK
	StatusPending    = sharedutil.StatusPending
	StatusError      = sharedutil.StatusError
	StatusRejected   = sharedutil.StatusRejected
	StatusProgrammed = sharedutil.StatusProgrammed
	StatusNotOwner   = sharedutil.StatusNotOwner
	StatusConflict   = sharedutil.StatusConflict
	StatusUnknown    = sharedutil.StatusUnknown

	DelegationComplete   = sharedutil.DelegationComplete
	DelegationPartial    = sharedutil.DelegationPartial
	DelegationIncomplete = sharedutil.DelegationIncomplete
	DelegationUnknown    = sharedutil.DelegationUnknown

	LabelManagedBy       = sharedutil.LabelManagedBy
	LabelDNSManaged      = sharedutil.LabelDNSManaged
	LabelSourceKind      = sharedutil.LabelSourceKind
	LabelSourceName      = sharedutil.LabelSourceName
	LabelSourceNamespace = sharedutil.LabelSourceNamespace

	ValueManagedByNetworking = sharedutil.ValueManagedByNetworking
	ValueDNSManaged          = sharedutil.ValueDNSManaged
	ValueSourceKindGateway   = sharedutil.ValueSourceKindGateway
)

// Delegation is the client-side comparison of assigned versus observed
// nameservers. See go.miloapis.com/dns-operator/internal/dns/util.Delegation.
type Delegation = sharedutil.Delegation

var (
	ZoneStatus         = sharedutil.ZoneStatus
	RecordStatus       = sharedutil.RecordStatus
	RecordStatusInZone = sharedutil.RecordStatusInZone
	DelegationState    = sharedutil.DelegationState
	MachineOwned       = sharedutil.MachineOwned
)
