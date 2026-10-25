// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"testing"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestCheckpointDropsChunksOlderThanTheAppliedPublication(t *testing.T) {
	state := newCheckpoint()
	state.Publications["zone"] = publicationState{Fence: fence{Epoch: 1_790_000_000, Revision: 3}}
	stage := func(manifest string, epoch, revision uint64) {
		state.Chunks[manifest] = map[string]model.PublicationChunk{chunkKey(0): {ManifestUID: manifest, ZoneUID: "zone", WriterEpoch: epoch, Revision: revision}}
	}
	stage("old-epoch", 181, 9)
	stage("older-revision", 1_790_000_000, 2)
	stage("applied", 1_790_000_000, 3)
	stage("pending", 1_790_000_000, 4)
	state.Chunks["unknown-zone"] = map[string]model.PublicationChunk{chunkKey(0): {ManifestUID: "unknown-zone", ZoneUID: "other", WriterEpoch: 1, Revision: 1}}

	pruneSupersededChunks(&state)
	loaded := state
	for manifest, kept := range map[string]bool{"old-epoch": false, "older-revision": false, "applied": true, "pending": true, "unknown-zone": true} {
		if _, ok := loaded.Chunks[manifest]; ok != kept {
			t.Errorf("chunks of %s kept=%v, want %v", manifest, ok, kept)
		}
	}
}
