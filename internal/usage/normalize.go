// SPDX-License-Identifier: AGPL-3.0-only

package usage

import (
	"strconv"
	"strings"

	"github.com/miekg/dns"
)

// NormalizeDomain lowercases a DNS name and strips a trailing dot so
// "Example.COM." and "example.com" compare equal.
func NormalizeDomain(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(name, ".")
	return strings.ToLower(name)
}

func recordTypeName(qtype uint32) string {
	if qtype > 0xffff {
		return ""
	}
	if s, ok := dns.TypeToString[uint16(qtype)]; ok && s != "" {
		return s
	}
	return dns.Type(qtype).String()
}

func rcodeName(rcode uint32) string {
	if rcode > 0xffff {
		// PowerDNS uses 65536 for a network error including a timeout.
		return "SERVFAIL"
	}
	if s, ok := dns.RcodeToString[int(rcode)]; ok && s != "" {
		return s
	}
	return "RCODE" + strconv.FormatUint(uint64(rcode), 10)
}

func parentDomain(name string) string {
	i := strings.IndexByte(name, '.')
	if i < 0 || i+1 >= len(name) {
		return ""
	}
	return name[i+1:]
}
