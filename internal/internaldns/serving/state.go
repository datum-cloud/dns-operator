// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type contributionFence struct {
	Generation uint64 `json:"generation"`
	Sequence   uint64 `json:"sequence"`
}

type replicaAck struct {
	Rejected   bool      `json:"rejected,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
	Fence      fence     `json:"fence"`
	ValidUntil time.Time `json:"validUntil"`
}

const checkpointFormatVersion = 6

type checkpoint struct {
	Active             *activeConfiguration                         `json:"active,omitempty"`
	FormatVersion      int                                          `json:"formatVersion"`
	ServingFence       fence                                        `json:"servingFence"`
	Snapshot           *model.ServingSnapshot                       `json:"snapshot,omitempty"`
	Publications       map[string]publicationState                  `json:"publications"`
	Chunks             map[string]map[string]model.PublicationChunk `json:"chunks"`
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
	state.UpdatedAt = time.Now().UTC()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	temp := s.path + ".tmp"
	if err := writeDurable(temp, b, 0o600); err != nil {
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
