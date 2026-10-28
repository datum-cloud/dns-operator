package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type SnapshotLister interface {
	List(context.Context, string) ([]model.Envelope, error)
}
type SnapshotBootstrap struct {
	Store         SnapshotLister
	Region, Shard string
	Attempts      int
	RetryDelay    time.Duration
}

// Load obtains a self-consistent bootstrap cut. Chunk keys are immutable and
// revision-qualified; stable manifest heads are accepted only when all of their
// referenced chunks are present and hash correctly in the same listing.
func (b SnapshotBootstrap) Load(ctx context.Context) ([]model.Envelope, error) {
	if b.Store == nil || b.Region == "" || b.Shard == "" {
		return nil, errors.New("snapshot store, region, and shard are required")
	}
	attempts := b.Attempts
	if attempts <= 0 {
		attempts = 4
	}
	delay := b.RetryDelay
	if delay <= 0 {
		delay = 100 * time.Millisecond
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		envs, err := b.Store.List(ctx, model.SnapshotPrefix(b.Region, b.Shard))
		if err == nil {
			envs, err = verifyBootstrap(envs, b.Region, b.Shard)
		}
		if err == nil {
			sort.SliceStable(envs, func(i, j int) bool { return bootstrapOrder(envs[i].Kind) < bootstrapOrder(envs[j].Kind) })
			return envs, nil
		}
		last = err
		if attempt+1 < attempts {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil, fmt.Errorf("consistent internal DNS bootstrap unavailable: %w", last)
}

func verifyBootstrap(envs []model.Envelope, region, shard string) ([]model.Envelope, error) {
	chunks := map[string][]model.PublicationChunk{}
	var manifests []model.PublicationManifest
	manifestByZone := map[string]model.PublicationManifest{}
	type servingCandidate struct {
		snapshot model.ServingSnapshot
		envelope model.Envelope
	}
	var servingCandidates []servingCandidate
	for _, env := range envs {
		if err := env.Validate(); err != nil {
			return nil, err
		}
		if env.Region != region || env.Shard != shard {
			return nil, errors.New("bootstrap contains a foreign region or shard")
		}
		switch env.Kind {
		case model.KindPublicationChunk:
			var c model.PublicationChunk
			if err := json.Unmarshal(env.Payload, &c); err != nil {
				return nil, err
			}
			if err := c.Validate(); err != nil {
				return nil, err
			}
			if c.ZoneUID != env.ResourceUID || c.WriterEpoch != env.Epoch || c.Revision != env.Revision {
				return nil, errors.New("bootstrap chunk envelope identity does not match payload")
			}
			chunks[c.ManifestUID] = append(chunks[c.ManifestUID], c)
		case model.KindPublicationManifest:
			var m model.PublicationManifest
			if err := json.Unmarshal(env.Payload, &m); err != nil {
				return nil, err
			}
			if err := m.Validate(); err != nil {
				return nil, err
			}
			if m.ZoneUID != env.ResourceUID || m.WriterEpoch != env.Epoch || m.Revision != env.Revision {
				return nil, errors.New("bootstrap manifest envelope identity does not match payload")
			}
			manifests = append(manifests, m)
			if old, ok := manifestByZone[m.ZoneUID]; !ok || model.Compare(m.WriterEpoch, m.Revision, old.WriterEpoch, old.Revision) > 0 {
				manifestByZone[m.ZoneUID] = m
			}
		case model.KindServingSnapshot:
			var snapshot model.ServingSnapshot
			if err := json.Unmarshal(env.Payload, &snapshot); err != nil {
				return nil, err
			}
			if err := snapshot.Validate(); err != nil {
				return nil, err
			}
			if snapshot.Region != env.Region || snapshot.Shard != env.Shard || snapshot.ConfigurationEpoch != env.Epoch || snapshot.ConfigurationRevision != env.Revision {
				return nil, errors.New("bootstrap serving envelope identity does not match payload")
			}
			servingCandidates = append(servingCandidates, servingCandidate{snapshot: snapshot, envelope: env})
		default:
			return nil, fmt.Errorf("bootstrap contains unsupported kind %q", env.Kind)
		}
	}
	if len(servingCandidates) == 0 {
		return nil, errors.New("bootstrap has no complete serving snapshot")
	}
	serving := servingCandidates[0]
	for _, candidate := range servingCandidates[1:] {
		if model.Compare(candidate.envelope.Epoch, candidate.envelope.Revision, serving.envelope.Epoch, serving.envelope.Revision) > 0 {
			serving = candidate
		}
	}
	for _, candidate := range servingCandidates {
		if model.Compare(candidate.envelope.Epoch, candidate.envelope.Revision, serving.envelope.Epoch, serving.envelope.Revision) == 0 && !equalJSON(candidate.snapshot, serving.snapshot) {
			return nil, errors.New("bootstrap has conflicting serving snapshots at the winning fence")
		}
	}
	for _, m := range manifests {
		if _, err := model.VerifyManifest(m, chunks[m.ManifestUID]); err != nil {
			return nil, err
		}
	}
	for _, binding := range serving.snapshot.Bindings {
		if binding.Tombstone || binding.ConfigurationPending {
			continue
		}
		for _, zone := range binding.Zones {
			if zone.Pending {
				continue
			}
			manifest, ok := manifestByZone[zone.ZoneUID]
			if !ok || model.Compare(manifest.WriterEpoch, manifest.Revision, zone.RequiredPublicationEpoch, zone.RequiredPublicationRevision) < 0 {
				return nil, fmt.Errorf("bootstrap has no current publication for attached zone %q", zone.ZoneUID)
			}
		}
	}
	manifestUIDs := map[string]bool{}
	for _, manifest := range manifests {
		manifestUIDs[manifest.ManifestUID] = true
	}
	filtered := make([]model.Envelope, 0, len(envs))
	for _, env := range envs {
		switch env.Kind {
		case model.KindServingSnapshot:
			continue
		case model.KindPublicationChunk:
			var chunk model.PublicationChunk
			if err := json.Unmarshal(env.Payload, &chunk); err != nil {
				return nil, err
			}
			if !manifestUIDs[chunk.ManifestUID] {
				continue
			}
		}
		filtered = append(filtered, env)
	}
	return append(filtered, serving.envelope), nil
}
func bootstrapOrder(k model.Kind) int {
	switch k {
	case model.KindPublicationChunk:
		return 0
	case model.KindServingSnapshot:
		return 1
	default:
		return 2
	}
}
