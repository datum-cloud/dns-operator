// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

const legacyObservationGrantUID = "legacy-checkpoint"

// contributionFence persists the complete immutable observation identity.
// The optional revision field supplies the sequence when the decoded fence omits it.
type contributionFence struct {
	GrantUID       string    `json:"grantUID,omitempty"`
	Epoch          uint64    `json:"epoch"`
	Sequence       uint64    `json:"sequence,omitempty"`
	ValidUntil     time.Time `json:"validUntil,omitempty"`
	LegacyRevision uint64    `json:"revision,omitempty"`
}

type replicaAck struct {
	ObservedAt time.Time `json:"observedAt"`
	Fence      fence     `json:"fence"`
	ValidUntil time.Time `json:"validUntil"`
}

const checkpointFormatVersion = 4

type checkpoint struct {
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
		if uid == "" || f.Identity == "" || f.ConfigurationGeneration == 0 || f.ConfigurationRevision == 0 || f.Authorization.IssuerEpoch == 0 || f.Authorization.Revision == 0 || f.Authorization.ValidUntil.IsZero() {
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
	if state.Snapshot != nil {
		if err := state.Snapshot.Validate(); err != nil {
			return state, fmt.Errorf("invalid persisted serving snapshot: %w", err)
		}
	}
	for uid, p := range state.Publications {
		if !p.Manifest.Tombstone {
			var err error
			p.Plan, err = migrateLegacyObservationFences(p.Plan)
			if err != nil {
				return state, fmt.Errorf("migrate persisted publication %s: %w", uid, err)
			}
			state.Publications[uid] = p
			if err := p.Plan.Validate(); err != nil {
				return state, fmt.Errorf("invalid persisted publication %s: %w", uid, err)
			}
		}
	}
	if err := migrateContributionHighWater(&state); err != nil {
		return state, err
	}
	return state, nil
}

func migrateLegacyObservationFences(plan model.PublicationPlan) (model.PublicationPlan, error) {
	if len(plan.ObservationFences) != 0 {
		return plan, nil
	}
	byUID := map[string]model.ObservationFence{}
	for _, rrset := range plan.RRSets {
		for _, record := range rrset.Records {
			if record.ContributionUID == "" {
				continue
			}
			next := model.ObservationFence{ContributionUID: record.ContributionUID, GrantUID: legacyObservationGrantUID, WriterEpoch: record.WriterEpoch, Sequence: record.Sequence, ValidUntil: record.ValidUntil}
			if old, ok := byUID[record.ContributionUID]; ok && (old.WriterEpoch != next.WriterEpoch || old.Sequence != next.Sequence || !old.ValidUntil.Equal(next.ValidUntil)) {
				return plan, fmt.Errorf("legacy contribution %q has inconsistent record fences", record.ContributionUID)
			}
			byUID[record.ContributionUID] = next
		}
	}
	for _, observation := range byUID {
		plan.ObservationFences = append(plan.ObservationFences, observation)
	}
	sort.Slice(plan.ObservationFences, func(i, j int) bool {
		return plan.ObservationFences[i].ContributionUID < plan.ObservationFences[j].ContributionUID
	})
	return plan, nil
}

func migrateContributionHighWater(state *checkpoint) error {
	proofs := map[string]model.ObservationFence{}
	for _, publication := range state.Publications {
		for _, observation := range publication.Plan.ObservationFences {
			old, ok := proofs[observation.ContributionUID]
			if !ok || model.Compare(observation.WriterEpoch, observation.Sequence, old.WriterEpoch, old.Sequence) > 0 {
				proofs[observation.ContributionUID] = observation
			}
		}
	}
	for uid, current := range state.ContributionFences {
		if current.Sequence == 0 {
			current.Sequence = current.LegacyRevision
		}
		current.LegacyRevision = 0
		if proof, ok := proofs[uid]; ok && proof.WriterEpoch == current.Epoch && proof.Sequence == current.Sequence {
			if current.ValidUntil.IsZero() {
				current.ValidUntil = proof.ValidUntil
			}
			if current.GrantUID == "" && proof.GrantUID != legacyObservationGrantUID {
				current.GrantUID = proof.GrantUID
			}
		}
		if current.Epoch == 0 || current.Sequence == 0 {
			return fmt.Errorf("invalid persisted contribution fence %q", uid)
		}
		state.ContributionFences[uid] = current
	}
	return nil
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
