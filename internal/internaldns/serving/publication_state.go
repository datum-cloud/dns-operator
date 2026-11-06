// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"maps"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

// tombstoneAckRetention bounds how long a verified withdrawal keeps renewing
// its acknowledgement. The control plane needs one fresh verified ack to
// finish a zone deletion and ignores acks for zones it no longer owns.
const tombstoneAckRetention = 24 * time.Hour

type fence struct {
	Epoch    uint64 `json:"epoch"`
	Revision uint64 `json:"revision"`
}

type publicationState struct {
	Fence         fence                     `json:"fence"`
	Manifest      model.PublicationManifest `json:"manifest"`
	Plan          model.PublicationPlan     `json:"plan"`
	LocalRevision uint64                    `json:"localRevision,omitempty"`
	EffectiveHash string                    `json:"effectiveHash,omitempty"`
	Verified      bool                      `json:"verified"`
	WithdrawnAt   time.Time                 `json:"withdrawnAt,omitzero"`
	Retired       bool                      `json:"retired,omitempty"`
}

// retireTombstones starts each verified withdrawal's retention clock and, once
// it has run out, keeps only the fence and manifest that reject an older
// replay of the zone.
func (a *Agent) retireTombstones(now time.Time) error {
	previous := map[string]publicationState{}
	for uid, p := range a.state.Publications {
		if !p.Manifest.Tombstone || !p.Verified || p.Retired {
			continue
		}
		next := p
		switch {
		case p.WithdrawnAt.IsZero():
			next.WithdrawnAt = now
		case now.Sub(p.WithdrawnAt) >= tombstoneAckRetention:
			next.Plan, next.EffectiveHash, next.Retired = model.PublicationPlan{}, "", true
		default:
			continue
		}
		previous[uid] = p
		a.state.Publications[uid] = next
	}
	if len(previous) == 0 {
		return nil
	}
	if err := a.store.Save(a.state); err != nil {
		maps.Copy(a.state.Publications, previous)
		return err
	}
	return nil
}
