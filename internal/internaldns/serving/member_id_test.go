// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"testing"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestResolveMemberIDFromPodOrdinal(t *testing.T) {
	env := func(value string) func(string) string {
		return func(name string) string {
			if name == "POD_NAME" {
				return value
			}
			return ""
		}
	}
	c := Config{MemberIDFrom: &MemberIDSource{Env: "POD_NAME", Prefix: "node-east-"}}
	c.Render.NodeDNSDist.Backends = []model.Backend{{Address: "::1", Port: 5300}, {MemberID: "other", Address: "::2", Port: 5300}}
	if err := c.ResolveMemberID(env("private-dns-node-12")); err != nil {
		t.Fatal(err)
	}
	if c.MemberID != "node-east-12" || c.ReplicaID != "node-east-12" {
		t.Fatalf("member %q replica %q", c.MemberID, c.ReplicaID)
	}
	if c.Render.NodeDNSDist.Backends[0].MemberID != "node-east-12" || c.Render.NodeDNSDist.Backends[1].MemberID != "other" {
		t.Fatalf("backends %#v", c.Render.NodeDNSDist.Backends)
	}

	for name, tc := range map[string]struct {
		config Config
		pod    string
	}{
		"both set":     {Config{MemberID: "x", MemberIDFrom: &MemberIDSource{Env: "POD_NAME", Prefix: "node-"}}, "pod-0"},
		"no prefix":    {Config{MemberIDFrom: &MemberIDSource{Env: "POD_NAME"}}, "pod-0"},
		"unset env":    {Config{MemberIDFrom: &MemberIDSource{Env: "POD_NAME", Prefix: "node-"}}, ""},
		"no ordinal":   {Config{MemberIDFrom: &MemberIDSource{Env: "POD_NAME", Prefix: "node-"}}, "pod-abc"},
		"leading zero": {Config{MemberIDFrom: &MemberIDSource{Env: "POD_NAME", Prefix: "node-"}}, "pod-01"},
		"no separator": {Config{MemberIDFrom: &MemberIDSource{Env: "POD_NAME", Prefix: "node-"}}, "pod"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tc.config.ResolveMemberID(env(tc.pod)); err == nil {
				t.Fatalf("resolved %q", tc.config.MemberID)
			}
		})
	}
}
