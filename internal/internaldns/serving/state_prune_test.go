// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"encoding/json"
	"os"
	"path/filepath"
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

	path := filepath.Join(t.TempDir(), "checkpoint.json")
	if err := (stateStore{path: path}).Save(state); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var written struct {
		Chunks map[string]json.RawMessage `json:"chunks"`
	}
	if err := json.Unmarshal(b, &written); err != nil {
		t.Fatal(err)
	}
	for manifest, kept := range map[string]bool{"old-epoch": false, "older-revision": false, "applied": true, "pending": true, "unknown-zone": true} {
		if _, ok := written.Chunks[manifest]; ok != kept {
			t.Errorf("saved chunks of %s kept=%v, want %v", manifest, ok, kept)
		}
	}
}
