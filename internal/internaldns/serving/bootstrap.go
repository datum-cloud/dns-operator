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
			err = verifyBootstrap(envs, b.Region, b.Shard)
		}
		if err == nil {
			envs, err = currentBootstrapEnvelopes(envs)
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

// currentBootstrapEnvelopes drops immutable chunk sets that are no longer
// referenced by a stable manifest head. The KV listing may retain them for
// replay/rollback safety, but a cold member must not checkpoint or apply them.
func currentBootstrapEnvelopes(envs []model.Envelope) ([]model.Envelope, error) {
	manifests := map[string]bool{}
	for _, env := range envs {
		if env.Kind != model.KindPublicationManifest {
			continue
		}
		var manifest model.PublicationManifest
		if err := json.Unmarshal(env.Payload, &manifest); err != nil {
			return nil, err
		}
		manifests[manifest.ManifestUID] = true
	}
	out := make([]model.Envelope, 0, len(envs))
	for _, env := range envs {
		if env.Kind == model.KindPublicationChunk {
			var chunk model.PublicationChunk
			if err := json.Unmarshal(env.Payload, &chunk); err != nil {
				return nil, err
			}
			if !manifests[chunk.ManifestUID] {
				continue
			}
		}
		out = append(out, env)
	}
	return out, nil
}

func verifyBootstrap(envs []model.Envelope, region, shard string) error {
	chunks := map[string][]model.PublicationChunk{}
	var manifests []model.PublicationManifest
	manifestByZone := map[string]model.PublicationManifest{}
	var serving *model.ServingSnapshot
	for _, env := range envs {
		if err := env.Validate(); err != nil {
			return err
		}
		if env.Region != region || env.Shard != shard {
			return errors.New("bootstrap contains a foreign region or shard")
		}
		switch env.Kind {
		case model.KindPublicationChunk:
			var c model.PublicationChunk
			if err := json.Unmarshal(env.Payload, &c); err != nil {
				return err
			}
			if err := c.Validate(); err != nil {
				return err
			}
			if c.ZoneUID != env.ResourceUID || c.WriterEpoch != env.Epoch || c.Revision != env.Revision {
				return errors.New("bootstrap chunk envelope identity does not match payload")
			}
			chunks[c.ManifestUID] = append(chunks[c.ManifestUID], c)
		case model.KindPublicationManifest:
			var m model.PublicationManifest
			if err := json.Unmarshal(env.Payload, &m); err != nil {
				return err
			}
			if err := m.Validate(); err != nil {
				return err
			}
			if m.ZoneUID != env.ResourceUID || m.WriterEpoch != env.Epoch || m.Revision != env.Revision {
				return errors.New("bootstrap manifest envelope identity does not match payload")
			}
			manifests = append(manifests, m)
			if old, ok := manifestByZone[m.ZoneUID]; !ok || model.Compare(m.WriterEpoch, m.Revision, old.WriterEpoch, old.Revision) > 0 {
				manifestByZone[m.ZoneUID] = m
			}
		case model.KindServingSnapshot:
			var snapshot model.ServingSnapshot
			if err := json.Unmarshal(env.Payload, &snapshot); err != nil {
				return err
			}
			if err := snapshot.Validate(); err != nil {
				return err
			}
			if snapshot.Region != env.Region || snapshot.Shard != env.Shard || snapshot.ConfigurationEpoch != env.Epoch || snapshot.ConfigurationRevision != env.Revision {
				return errors.New("bootstrap serving envelope identity does not match payload")
			}
			if serving != nil {
				return errors.New("bootstrap has multiple serving snapshots")
			}
			serving = &snapshot
		default:
			return fmt.Errorf("bootstrap contains unsupported kind %q", env.Kind)
		}
	}
	if serving == nil {
		return errors.New("bootstrap has no complete serving snapshot")
	}
	for _, m := range manifests {
		if _, err := model.VerifyManifest(m, chunks[m.ManifestUID]); err != nil {
			return err
		}
	}
	for _, binding := range serving.Bindings {
		if binding.Tombstone || binding.ConfigurationPending {
			continue
		}
		for _, zone := range binding.Zones {
			if zone.Pending {
				continue
			}
			manifest, ok := manifestByZone[zone.ZoneUID]
			if !ok || model.Compare(manifest.WriterEpoch, manifest.Revision, zone.RequiredPublicationEpoch, zone.RequiredPublicationRevision) < 0 {
				return fmt.Errorf("bootstrap has no current publication for attached zone %q", zone.ZoneUID)
			}
		}
	}
	return nil
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
