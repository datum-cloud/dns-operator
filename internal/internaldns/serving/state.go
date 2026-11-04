// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type contributionFence struct {
	Generation uint64 `json:"generation"`
	Sequence   uint64 `json:"sequence"`
	Retired    bool   `json:"retired,omitempty"`
}

type replicaAck struct {
	Rejected   bool      `json:"rejected,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
	Fence      fence     `json:"fence"`
	ValidUntil time.Time `json:"validUntil"`
}

const checkpointFormatVersion = 7

type checkpoint struct {
	Active             *activeConfiguration                         `json:"active,omitempty"`
	FormatVersion      int                                          `json:"formatVersion"`
	ServingFence       fence                                        `json:"servingFence"`
	Snapshot           *model.ServingSnapshot                       `json:"snapshot,omitempty"`
	Publications       map[string]publicationState                  `json:"publications,omitempty"`
	Chunks             map[string]map[string]model.PublicationChunk `json:"chunks,omitempty"`
	Acks               map[string]map[string]replicaAck             `json:"acks"`
	BindingFences      map[string]bindingFence                      `json:"bindingFences"`
	ContributionFences map[string]contributionFence                 `json:"contributionFences"`
	RenderedHash       string                                       `json:"renderedHash,omitempty"`
	UpdatedAt          time.Time                                    `json:"updatedAt"`
}

func newCheckpoint() checkpoint {
	return checkpoint{FormatVersion: checkpointFormatVersion, Publications: map[string]publicationState{}, Chunks: map[string]map[string]model.PublicationChunk{}, Acks: map[string]map[string]replicaAck{}, ContributionFences: map[string]contributionFence{}, BindingFences: map[string]bindingFence{}}
}

type stateStore struct{ path string }

func (s stateStore) Load() (checkpoint, error) {
	state := newCheckpoint()
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state.FormatVersion = 0
	state.BindingFences = nil
	if err := json.Unmarshal(b, &state); err != nil {
		return state, fmt.Errorf("decode checkpoint: %w", err)
	}
	if state.FormatVersion != checkpointFormatVersion {
		return state, fmt.Errorf("unsupported checkpoint format %d: preserve the existing checkpoint; automatic backend or binding authority history migration is not supported", state.FormatVersion)
	}
	if state.BindingFences == nil {
		return state, errors.New("checkpoint lacks required binding authority history")
	}
	for uid, f := range state.BindingFences {
		if uid == "" || f.Identity == "" || f.ConfigurationGeneration == 0 || f.ConfigurationRevision == 0 || f.Version.Epoch == 0 || f.Version.Sequence == 0 {
			return state, fmt.Errorf("checkpoint binding %s has incomplete authority history", uid)
		}
	}
	if state.Snapshot != nil {
		for _, b := range state.Snapshot.Bindings {
			f, ok := state.BindingFences[b.BindingUID]
			if !ok || !equalJSON(f, bindingFenceFor(b)) {
				return state, fmt.Errorf("checkpoint binding %s does not match its authority history", b.BindingUID)
			}
		}
	}
	if state.Publications == nil {
		state.Publications = map[string]publicationState{}
	}
	if state.Chunks == nil {
		state.Chunks = map[string]map[string]model.PublicationChunk{}
	}
	pruneSupersededChunks(&state)
	if state.Acks == nil {
		state.Acks = map[string]map[string]replicaAck{}
	}
	if state.ContributionFences == nil {
		state.ContributionFences = map[string]contributionFence{}
	}
	for uid, observation := range state.ContributionFences {
		if uid == "" || observation.Generation == 0 || observation.Sequence == 0 {
			return state, fmt.Errorf("invalid persisted contribution fence %q", uid)
		}
	}
	if state.Snapshot != nil {
		if err := state.Snapshot.Validate(); err != nil {
			return state, fmt.Errorf("invalid persisted serving snapshot: %w", err)
		}
	}
	for uid, p := range state.Publications {
		if !p.Manifest.Tombstone {
			if err := p.Plan.Validate(); err != nil {
				return state, fmt.Errorf("invalid persisted publication %s: %w", uid, err)
			}
		}
	}
	return state, nil
}

func (s stateStore) Save(state checkpoint) error {
	if state.FormatVersion != checkpointFormatVersion {
		return fmt.Errorf("cannot write unsupported checkpoint format %d", state.FormatVersion)
	}
	pruneSupersededChunks(&state)
	state.UpdatedAt = time.Now().UTC()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	// Encode straight to the file: a checkpoint holds every publication, and
	// a second in-memory copy of it on each save is what pushes a recovering
	// agent toward its memory limit.
	temp := s.path + ".tmp"
	if err := writeCheckpointDurable(temp, state, 0o640); err != nil {
		return err
	}
	if err := os.Rename(temp, s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func chunkKey(index int) string { return fmt.Sprint(index) }

func prepareServingState(dir string) error {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	lock, err := os.OpenFile(activationMarkerPath(dir)+".lock", os.O_CREATE|os.O_RDWR, 0640)
	if err != nil {
		return err
	}
	if err := lock.Chmod(0640); err != nil {
		_ = lock.Close()
		return err
	}
	if err := lock.Close(); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(dir, "checkpoint.json"), 0640); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// pruneSupersededChunks drops staged chunks of every publication older than
// its zone's applied fence. A chunk is needed only to verify a manifest that
// has not been applied yet, and a manifest older than the fence is never
// applied, so keeping them only grows the checkpoint.
func pruneSupersededChunks(state *checkpoint) {
	for manifest, chunks := range state.Chunks {
		for _, c := range chunks {
			applied := state.Publications[c.ZoneUID].Fence
			if applied.Revision > 0 && model.Compare(c.WriterEpoch, c.Revision, applied.Epoch, applied.Revision) < 0 {
				delete(state.Chunks, manifest)
			}
			break
		}
		if len(chunks) == 0 {
			delete(state.Chunks, manifest)
		}
	}
}

func writeCheckpointDurable(path string, state checkpoint, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	if err := encodeCheckpoint(w, state); err != nil {
		_ = f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// encodeCheckpoint writes the checkpoint's publications and chunks one entry
// at a time. They hold nearly all of its bytes, and encoding the whole
// checkpoint at once holds a second, growing copy of it on every save, which
// is what pushes a recovering agent toward its memory limit.
func encodeCheckpoint(w io.Writer, state checkpoint) error {
	publications, chunks := state.Publications, state.Chunks
	state.Publications, state.Chunks = nil, nil
	head, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if _, err := w.Write(head[:len(head)-1]); err != nil {
		return err
	}
	if err := encodeMapField(w, "publications", publications); err != nil {
		return err
	}
	if err := encodeMapField(w, "chunks", chunks); err != nil {
		return err
	}
	_, err = io.WriteString(w, "}")
	return err
}

func encodeMapField[V any](w io.Writer, name string, entries map[string]V) error {
	if _, err := fmt.Fprintf(w, ",%q:{", name); err != nil {
		return err
	}
	for i, key := range slices.Sorted(maps.Keys(entries)) {
		k, err := json.Marshal(key)
		if err != nil {
			return err
		}
		v, err := json.Marshal(entries[key])
		if err != nil {
			return err
		}
		if i > 0 {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		if _, err := w.Write(append(append(k, ':'), v...)); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "}")
	return err
}
