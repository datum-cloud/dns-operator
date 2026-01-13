package controller

const (
	CondAccepted   = "Accepted"
	CondProgrammed = "Programmed"
	CondDiscovered = "Discovered"

	ReasonAccepted            = "Accepted"
	ReasonPending             = "Pending"
	ReasonInvalidDNSRecordSet = "InvalidDNSRecordSet"
	ReasonInvalidSecret       = "InvalidSecret"
	ReasonProgrammed          = "Programmed"
	ReasonDiscovered          = "Discovered"
	ReasonDNSZoneInUse        = "DNSZoneInUse"
	ReasonNotOwner            = "NotOwner"
	ReasonPDNSError           = "PDNSError"
)
