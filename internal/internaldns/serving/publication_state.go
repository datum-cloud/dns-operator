// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

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
}
