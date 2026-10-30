// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"encoding/json"
	"testing"

	contextdns "go.miloapis.com/dns-operator/internal/internaldns/coredns"
)

func TestRenderUDPReadBuffer(t *testing.T) {
	c, s := renderFixture(t)
	c.NodeDNSDist.IdentityKeyFile = "/run/identity.key"
	c.NodeCoreDNS.IdentityKeyFile = "/run/identity.key"
	c.NodeCoreDNS.UDPReadBufferBytes = 1048576
	r, err := Render(c, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	var actual contextdns.Config
	if err := json.Unmarshal(r.Files[c.NodeCoreDNS.Path], &actual); err != nil {
		t.Fatal(err)
	}
	if actual.UDPReadBufferBytes != 1048576 {
		t.Fatalf("lost buffer size: %d", actual.UDPReadBufferBytes)
	}
	c.NodeCoreDNS.UDPReadBufferBytes = -1
	if _, err := Render(c, s, nil); err == nil {
		t.Fatal("negative read buffer accepted")
	}
}
