// SPDX-License-Identifier: AGPL-3.0-only

package usage

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.miloapis.com/billing/emission"

	"go.miloapis.com/dns-operator/internal/config"
)

type recordingRecorder struct {
	mu     sync.Mutex
	events []emission.UsageEvent
	err    error
}

func (r *recordingRecorder) Record(_ context.Context, ev emission.UsageEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	if ev.Dimensions != nil {
		dims := make(map[string]string, len(ev.Dimensions))
		for k, v := range ev.Dimensions {
			dims[k] = v
		}
		ev.Dimensions = dims
	}
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingRecorder) snapshot() []emission.UsageEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]emission.UsageEvent, len(r.events))
	copy(out, r.events)
	return out
}

func TestNewRecorderNoopWhenDisabled(t *testing.T) {
	t.Parallel()
	r, err := NewRecorder(config.UsageConfig{Enabled: false, Endpoint: "http://localhost:9880/cloudevents"})
	require.NoError(t, err)
	assert.IsType(t, emission.NoopRecorder{}, r)
}

func TestNewRecorderNoopWhenEndpointEmpty(t *testing.T) {
	t.Parallel()
	r, err := NewRecorder(config.UsageConfig{Enabled: true, Endpoint: ""})
	require.NoError(t, err)
	assert.IsType(t, emission.NoopRecorder{}, r)
}
