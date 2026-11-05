// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"errors"
	"fmt"
	"strings"
)

// MemberIDSource derives a member's ID from its Pod, so every member of a
// StatefulSet shares one configuration. The ID is Prefix followed by the
// ordinal that ends the name in Env.
type MemberIDSource struct {
	Env    string `json:"env"`
	Prefix string `json:"prefix"`
}

// ResolveMemberID sets the member and replica IDs from MemberIDFrom, and names
// this member on any of its own dnsdist backends that leave the member unset.
func (c *Config) ResolveMemberID(getenv func(string) string) error {
	if c.MemberIDFrom == nil {
		return nil
	}
	source := *c.MemberIDFrom
	if c.MemberID != "" || c.ReplicaID != "" {
		return errors.New("memberIDFrom replaces memberID and replicaID; set only one")
	}
	if source.Env == "" || source.Prefix == "" {
		return errors.New("memberIDFrom requires env and prefix")
	}
	name := getenv(source.Env)
	ordinal := name[strings.LastIndex(name, "-")+1:]
	if !isOrdinal(ordinal) {
		return fmt.Errorf("memberIDFrom: %s=%q does not end in a Pod ordinal", source.Env, name)
	}
	c.MemberID = source.Prefix + ordinal
	c.ReplicaID = c.MemberID
	for i := range c.Render.NodeDNSDist.Backends {
		if c.Render.NodeDNSDist.Backends[i].MemberID == "" {
			c.Render.NodeDNSDist.Backends[i].MemberID = c.MemberID
		}
	}
	return nil
}

func isOrdinal(s string) bool {
	if s == "" || len(s) > 1 && s[0] == '0' {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
