// SPDX-License-Identifier: AGPL-3.0-only

package model

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
)

func SnapshotPrefix(region, shard string) string {
	return SafeToken(region) + "." + SafeToken(shard) + "."
}

func PublicationActivationName(manifest, region, shard string) string {
	name := manifest + "-" + SafeToken(region) + "-" + SafeToken(shard) + "-activate"
	if len(name) <= 63 {
		return strings.Trim(name, "-")
	}
	sum := sha256.Sum256([]byte(name))
	return strings.Trim(name[:50], "-") + fmt.Sprintf("-%x", sum[:6])
}

// Chunk keys retain a complete immutable revision until its manifest head has
// moved and the supported replay window has elapsed. A manifest head is written
// only after every referenced chunk is durable in both JetStream and bootstrap.
func SnapshotKey(env Envelope) (string, error) {
	prefix := SnapshotPrefix(env.Region, env.Shard)
	uid := OpaqueToken(env.ResourceUID)
	switch env.Kind {
	case KindPublicationChunk:
		var c PublicationChunk
		if err := json.Unmarshal(env.Payload, &c); err != nil {
			return "", err
		}
		return fmt.Sprintf("%schunk.%s.e%d.r%d.i%d", prefix, uid, env.Epoch, env.Revision, c.Index), nil
	case KindPublicationManifest:
		return prefix + "manifest." + uid, nil
	case KindServingSnapshot:
		return prefix + "serving." + uid, nil
	default:
		return "", fmt.Errorf("event %s is not bootstrap state", env.Kind)
	}
}
