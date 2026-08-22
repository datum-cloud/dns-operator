// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import (
	"fmt"
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// Fields returns the value of e broken out as ordered label/value pairs, for
// the describe view. A record entered as presentation format is shown by its
// named fields, the same way a record entered by flags is echoed back as
// presentation format — each notation teaches the other.
//
// Fields returns nil when the entry carries no value for t.
func Fields(t dnsv1alpha1.RRType, e dnsv1alpha1.RecordEntry) [][2]string {
	switch t {
	case dnsv1alpha1.RRTypeA:
		if e.A == nil {
			return nil
		}
		return [][2]string{{"Address", e.A.Content}}

	case dnsv1alpha1.RRTypeAAAA:
		if e.AAAA == nil {
			return nil
		}
		return [][2]string{{"Address", e.AAAA.Content}}

	case dnsv1alpha1.RRTypeCNAME:
		if e.CNAME == nil {
			return nil
		}
		return [][2]string{{"Target", qualify(e.CNAME.Content)}}

	case dnsv1alpha1.RRTypeALIAS:
		if e.ALIAS == nil {
			return nil
		}
		return [][2]string{{"Target", qualify(e.ALIAS.Content)}}

	case dnsv1alpha1.RRTypeNS:
		if e.NS == nil {
			return nil
		}
		return [][2]string{{"Nameserver", qualify(e.NS.Content)}}

	case dnsv1alpha1.RRTypePTR:
		if e.PTR == nil {
			return nil
		}
		return [][2]string{{"Target", qualify(e.PTR.Content)}}

	case dnsv1alpha1.RRTypeTXT:
		if e.TXT == nil {
			return nil
		}
		// Decoded, so describe shows what the user wrote rather than the
		// escaped, chunked form the API stores.
		data := txtLogical(e.TXT.Content)
		out := [][2]string{{"Data", data}}
		if parts := chunk255(data); len(parts) > 1 {
			out = append(out, [2]string{"Strings", fmt.Sprintf("%d (chunked at 255 bytes)", len(parts))})
		}
		return out

	case dnsv1alpha1.RRTypeMX:
		if e.MX == nil {
			return nil
		}
		return [][2]string{
			{"Preference", fmt.Sprintf("%d", e.MX.Preference)},
			{"Exchange", qualify(e.MX.Exchange)},
		}

	case dnsv1alpha1.RRTypeSRV:
		if e.SRV == nil {
			return nil
		}
		return [][2]string{
			{"Priority", fmt.Sprintf("%d", e.SRV.Priority)},
			{"Weight", fmt.Sprintf("%d", e.SRV.Weight)},
			{"Port", fmt.Sprintf("%d", e.SRV.Port)},
			{"Target", qualify(e.SRV.Target)},
		}

	case dnsv1alpha1.RRTypeCAA:
		if e.CAA == nil {
			return nil
		}
		return [][2]string{
			{"Flag", fmt.Sprintf("%d", e.CAA.Flag)},
			{"Tag", e.CAA.Tag},
			{"Value", e.CAA.Value},
		}

	case dnsv1alpha1.RRTypeTLSA:
		if e.TLSA == nil {
			return nil
		}
		return [][2]string{
			{"Usage", fmt.Sprintf("%d", e.TLSA.Usage)},
			{"Selector", fmt.Sprintf("%d", e.TLSA.Selector)},
			{"Matching type", fmt.Sprintf("%d", e.TLSA.MatchingType)},
			{"Certificate data", e.TLSA.CertData},
		}

	case dnsv1alpha1.RRTypeHTTPS:
		if e.HTTPS == nil {
			return nil
		}
		return svcbFields(*e.HTTPS)

	case dnsv1alpha1.RRTypeSVCB:
		if e.SVCB == nil {
			return nil
		}
		return svcbFields(*e.SVCB)

	case dnsv1alpha1.RRTypeSOA:
		if e.SOA == nil {
			return nil
		}
		s := *e.SOA
		return [][2]string{
			{"Primary nameserver", qualify(s.MName)},
			{"Responsible party", qualify(s.RName)},
			{"Serial", soaNumField(s.Serial, soaSerial(0))},
			{"Refresh", soaNumField(s.Refresh, fmt.Sprintf("%d", soaDefaultRefresh))},
			{"Retry", soaNumField(s.Retry, fmt.Sprintf("%d", soaDefaultRetry))},
			{"Expire", soaNumField(s.Expire, fmt.Sprintf("%d", soaDefaultExpire))},
			{"Minimum TTL", soaNumField(s.TTL, fmt.Sprintf("%d", soaDefaultMinimum))},
		}
	}
	return nil
}

// soaNumField renders an unset (zero) SOA field as the default the backend will
// substitute, annotated so the number is never a mystery.
func soaNumField(v uint32, def string) string {
	if v == 0 {
		return def + " (default)"
	}
	return fmt.Sprintf("%d", v)
}

func svcbFields(s dnsv1alpha1.HTTPSRecordSpec) [][2]string {
	target := strings.TrimSpace(s.Target)
	if target == "" {
		target = "."
	}
	if target != "." {
		target = qualify(target)
	}
	mode := "service"
	if s.Priority == 0 {
		mode = "alias"
	}
	out := [][2]string{
		{"Priority", fmt.Sprintf("%d (%s mode)", s.Priority, mode)},
		{"Target", target},
	}
	if s.Priority != 0 {
		for _, k := range sortedKeys(s.Params) {
			out = append(out, [2]string{"Param " + k, s.Params[k]})
		}
	}
	return out
}
