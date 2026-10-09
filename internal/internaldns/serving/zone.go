package serving

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

// publicationSerial proves the exact publication and locally enforced expiry
// content. It is unrelated to a wall clock and cannot renew contributor leases.
func publicationSerial(p publicationState, now time.Time) uint32 {
	hash := publicationFingerprint(p, now)
	serial, _ := strconv.ParseUint(hash[:8], 16, 32)
	if serial == 0 {
		serial = 1
	}
	return uint32(serial)
}

func publicationFingerprint(p publicationState, now time.Time) string {
	data, _ := json.Marshal(p.Plan)
	return model.Hash([]byte(fmt.Sprintf("%d/%d/%d/%s/%s", p.Fence.Epoch, p.Fence.Revision, p.LocalRevision, model.Hash(data), effectivePlanHash(p.Plan, now))))
}
func publicationMailbox(p publicationState, now time.Time) string {
	hash := publicationFingerprint(p, now)
	return hash[:32] + "." + hash[32:] + ".publication.internal."
}
func renderZone(p publicationState, now time.Time) ([]byte, error) {
	var out bytes.Buffer
	apex := model.AbsoluteName(p.Plan.Apex)
	fmt.Fprintf(&out, "$ORIGIN %s\n$TTL 5\n@ IN SOA ns1.internal-dns.invalid. %s %d 60 30 3600 5\n@ IN NS ns1.internal-dns.invalid.\n", apex, publicationMailbox(p, now), publicationSerial(p, now))
	ownerHasRecord := map[string]bool{}
	for _, rrset := range p.Plan.RRSets {
		if strings.EqualFold(rrset.Type, "SOA") {
			return nil, fmt.Errorf("publication SOA is reserved for version proof")
		}
		for _, record := range rrset.Records {
			if !record.Eligible || (record.ContributionUID != "" && !now.Before(record.ValidUntil)) {
				continue
			}
			line := fmt.Sprintf("%s %d IN %s %s", model.AbsoluteName(rrset.Name), rrset.TTL, strings.ToUpper(rrset.Type), record.Content)
			rr, err := dns.NewRR(line)
			if err != nil {
				return nil, fmt.Errorf("invalid zone record: %w", err)
			}
			fmt.Fprintln(&out, rr.String())
			ownerHasRecord[model.AbsoluteName(rrset.Name)] = true
		}
	}
	for _, owner := range p.Plan.Owners {
		owner = model.AbsoluteName(owner)
		if !ownerHasRecord[owner] {
			if _, ok := dns.IsDomainName("_dns-ownership." + owner); !ok {
				return nil, fmt.Errorf("owned name too long for NODATA sentinel: %s", owner)
			}
			fmt.Fprintf(&out, "_dns-ownership.%s 5 IN TXT \"owned\"\n", owner)
		}
	}
	return out.Bytes(), nil
}
