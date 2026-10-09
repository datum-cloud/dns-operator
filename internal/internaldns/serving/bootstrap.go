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
	serving := false
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
			chunks[c.ManifestUID] = append(chunks[c.ManifestUID], c)
		case model.KindPublicationManifest:
			var m model.PublicationManifest
			if err := json.Unmarshal(env.Payload, &m); err != nil {
				return err
			}
			manifests = append(manifests, m)
		case model.KindServingSnapshot:
			serving = true
		default:
			return fmt.Errorf("bootstrap contains unsupported kind %q", env.Kind)
		}
	}
	if !serving {
		return errors.New("bootstrap has no complete serving snapshot")
	}
	for _, m := range manifests {
		if _, err := model.VerifyManifest(m, chunks[m.ManifestUID]); err != nil {
			return err
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
