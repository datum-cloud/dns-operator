// SPDX-License-Identifier: AGPL-3.0-only

package usage

import (
	"fmt"

	"go.miloapis.com/billing/emission"

	"go.miloapis.com/dns-operator/internal/config"
)

// NewRecorder returns a billing emission recorder. Disabled configs and
// missing endpoints yield a NoopRecorder so local/e2e runs stay quiet.
func NewRecorder(cfg config.UsageConfig) (emission.Recorder, error) {
	if !cfg.Enabled || cfg.Endpoint == "" {
		return emission.NoopRecorder{}, nil
	}
	r, err := emission.NewUsageRecorder(emission.WithEndpoint(cfg.Endpoint))
	if err != nil {
		return nil, fmt.Errorf("constructing usage recorder: %w", err)
	}
	return r, nil
}
