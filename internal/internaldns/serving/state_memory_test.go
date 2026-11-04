// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

// recoveryCheckpoint builds the state an agent restarts with after a
// control-plane restart: every zone applied, plus the republished revision of
// each staged as chunks and not yet applied.
func recoveryCheckpoint(t testing.TB, zones, records int) checkpoint {
	t.Helper()
	state := newCheckpoint()
	now := time.Now().UTC()
	for z := range zones {
		zoneUID := fmt.Sprintf("zone-%04d", z)
		apex := fmt.Sprintf("z%04d.internal", z)
		plan := model.PublicationPlan{ZoneUID: zoneUID, Apex: apex, ObservationFences: []model.ObservationFence{{ContributionUID: "contrib", Sequence: 1, Generation: 1}}}
		for r := range records {
			name := fmt.Sprintf("host-%04d.%s", r, apex)
			plan.Owners = append(plan.Owners, name)
			plan.RRSets = append(plan.RRSets, model.RRSet{Name: name, Type: "A", TTL: 30, Records: []model.RRRecord{{Content: fmt.Sprintf("10.%d.%d.%d", z%250, r/250, r%250+1), ContributionUID: "contrib", Sequence: 1, Eligible: true, Generation: 1}}})
		}
		payload, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		applied := fence{Epoch: 2, Revision: 4}
		manifest := model.PublicationManifest{ManifestUID: zoneUID + "-4", ZoneUID: zoneUID, Apex: apex, WriterEpoch: applied.Epoch, Revision: applied.Revision, Chunks: []model.ChunkRef{{Index: 0, SHA256: model.Hash(payload), Size: len(payload)}}, ContentHash: model.Hash(payload), GeneratedAt: now}
		state.Publications[zoneUID] = publicationState{Fence: applied, Manifest: manifest, Plan: plan, Verified: true}
		staged := zoneUID + "-5"
		state.Chunks[staged] = map[string]model.PublicationChunk{chunkKey(0): {ManifestUID: staged, ZoneUID: zoneUID, WriterEpoch: 2, Revision: 5, SHA256: model.Hash(payload), Payload: payload}}
		state.Acks[zoneUID] = map[string]replicaAck{"regional-0/": {Fence: applied, ObservedAt: now, ValidUntil: now.Add(time.Minute)}}
	}
	return state
}

func allocatedDuring(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// peakHeapDuring samples the heap while f runs, with the collector kept
// aggressive so the heap tracks live data. Sampling can miss a short peak, so
// it understates rather than overstates.
func peakHeapDuring(f func()) uint64 {
	defer debug.SetGCPercent(debug.SetGCPercent(5))
	base := liveHeap()
	var peak atomic.Uint64
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Microsecond):
			}
		}
	}()
	f()
	close(stop)
	<-stopped
	return peak.Load() - min(base, peak.Load())
}

func liveHeap() uint64 {
	var m runtime.MemStats
	// A second collection frees pooled buffers the first one only emptied.
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// A restarted agent loads its checkpoint and then saves it again for changes
// in the republish burst. Both must stay a small multiple of the checkpoint's
// size, or a large fleet state cannot fit under the agent's memory limit while
// it recovers.
func TestRecoveryCheckpointMemoryStaysProportional(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a large checkpoint")
	}
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	store := stateStore{path: path}
	if err := store.Save(recoveryCheckpoint(t, 400, 100)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	size := uint64(info.Size())

	baseline := liveHeap()
	var state checkpoint
	loadAlloc := allocatedDuring(func() {
		state, err = store.Load()
	})
	if err != nil {
		t.Fatal(err)
	}
	held := liveHeap()
	retained := held - min(baseline, held)
	savePeak := peakHeapDuring(func() {
		for range 5 {
			if err := store.Save(state); err != nil {
				t.Fatal(err)
			}
		}
	})
	runtime.KeepAlive(state)
	t.Logf("checkpoint %d KiB: load allocates %.1fx, holds %.1fx; saves add %.2fx at peak", size>>10, float64(loadAlloc)/float64(size), float64(retained)/float64(size), float64(savePeak)/float64(size))
	for _, check := range []struct {
		what  string
		bytes uint64
		limit float64
	}{
		{"loading allocates", loadAlloc, 8},
		{"the loaded state holds", retained, 4},
		// Saving encoded the whole checkpoint in memory, about 3x its size
		// at peak; it now holds one entry at a time.
		{"saving adds", savePeak, 0.75},
	} {
		if ratio := float64(check.bytes) / float64(size); ratio > check.limit {
			t.Errorf("%s %.1fx the checkpoint size, over the %.2gx budget", check.what, ratio, check.limit)
		}
	}
}

func BenchmarkCheckpointSave(b *testing.B) {
	state := recoveryCheckpoint(b, 400, 100)
	store := stateStore{path: filepath.Join(b.TempDir(), "checkpoint.json")}
	b.ReportAllocs()
	for b.Loop() {
		if err := store.Save(state); err != nil {
			b.Fatal(err)
		}
	}
}

func TestStreamedCheckpointMatchesWholeEncoding(t *testing.T) {
	state := recoveryCheckpoint(t, 3, 2)
	var streamed bytes.Buffer
	if err := encodeCheckpoint(&streamed, state); err != nil {
		t.Fatal(err)
	}
	whole, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(streamed.Bytes(), &got); err != nil {
		t.Fatalf("streamed checkpoint is not JSON: %v", err)
	}
	if err := json.Unmarshal(whole, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("streamed checkpoint differs from encoding it whole")
	}

	path := filepath.Join(t.TempDir(), "checkpoint.json")
	if err := (stateStore{path: path}).Save(newCheckpoint()); err != nil {
		t.Fatal(err)
	}
	loaded, err := (stateStore{path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Publications == nil || loaded.Chunks == nil {
		t.Fatal("omitted empty maps did not load as empty maps")
	}
}
