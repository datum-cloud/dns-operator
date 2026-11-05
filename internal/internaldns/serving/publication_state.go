// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
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
	changed := false
	for uid, p := range a.state.Publications {
		if !p.Manifest.Tombstone || !p.Verified || p.Retired {
			continue
		}
		switch {
		case p.WithdrawnAt.IsZero():
			p.WithdrawnAt = now
		case now.Sub(p.WithdrawnAt) >= tombstoneAckRetention:
			p.Plan, p.EffectiveHash, p.Retired = model.PublicationPlan{}, "", true
		default:
			continue
		}
		a.state.Publications[uid] = p
		changed = true
	}
	if !changed {
		return nil
	}
	return a.store.Save(a.state)
}
