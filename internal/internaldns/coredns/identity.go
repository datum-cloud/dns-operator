// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/netip"
	"os"
	"strings"

	"github.com/miekg/dns"
)

// IdentityOption carries the local resolver's key followed by the context
// destination. It lets dnsdist reuse backend connections, which a per-query
// PROXY header prevents.
const IdentityOption uint16 = 65431

func LoadIdentityKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(string(data))
	if decoded, err := base64.StdEncoding.DecodeString(key); err != nil || len(decoded) != 32 {
		return nil, errors.New("identity key must be 32 base64-encoded bytes")
	}
	return []byte(key), nil
}

// identityDestination removes every identity option from r. It reports
// whether any was present and the destination asserted by the single option
// that carries the key. A client can add the option itself, so an option
// without the key is never trusted.
func identityDestination(r *dns.Msg, key []byte) (netip.Addr, bool) {
	var destination netip.Addr
	present, valid := false, 0
	for _, rr := range r.Extra {
		opt, ok := rr.(*dns.OPT)
		if !ok {
			continue
		}
		kept := opt.Option[:0]
		for _, option := range opt.Option {
			local, ok := option.(*dns.EDNS0_LOCAL)
			if !ok || local.Code != IdentityOption {
				kept = append(kept, option)
				continue
			}
			present = true
			if len(local.Data) <= len(key) || subtle.ConstantTimeCompare(local.Data[:len(key)], key) != 1 {
				continue
			}
			addr, err := netip.ParseAddr(string(local.Data[len(key):]))
			if err != nil {
				continue
			}
			destination = addr.Unmap()
			valid++
		}
		opt.Option = kept
	}
	if !present {
		return netip.Addr{}, false
	}
	if valid != 1 {
		return netip.Addr{}, true
	}
	return destination, true
}
