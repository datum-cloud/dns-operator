// SPDX-License-Identifier: AGPL-3.0-only

package transport

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

var (
	ErrKVNotFound = errors.New("key not found")
	ErrKVExists   = errors.New("key exists")
	ErrKVConflict = errors.New("key revision mismatch")
)

// KVEvent is one bucket change. Value is nil for a deletion.
type KVEvent struct {
	Key      string
	Value    []byte
	Revision uint64
}

// KVBucket is a compare-and-swap key-value bucket. Revisions increase across the
// whole bucket, so a newer revision of a key always compares greater.
type KVBucket struct{ kv jetstream.KeyValue }

func (j *JetStream) KeyValueBucket(ctx context.Context, bucket string, replicas int) (*KVBucket, error) {
	if bucket == "" {
		return nil, errors.New("bucket is required")
	}
	kv, err := j.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket, Description: "internal DNS control-plane publication state", History: 1, Storage: jetstream.FileStorage, Replicas: replicas})
	if err != nil {
		return nil, fmt.Errorf("ensure bucket %q: %w", bucket, err)
	}
	return &KVBucket{kv: kv}, nil
}

func (b *KVBucket) Get(ctx context.Context, key string) ([]byte, uint64, error) {
	entry, err := b.kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, 0, ErrKVNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	return entry.Value(), entry.Revision(), nil
}

func (b *KVBucket) Create(ctx context.Context, key string, value []byte) (uint64, error) {
	revision, err := b.kv.Create(ctx, key, value)
	if errors.Is(err, jetstream.ErrKeyExists) {
		return 0, ErrKVExists
	}
	return revision, err
}

func (b *KVBucket) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	next, err := b.kv.Update(ctx, key, value, revision)
	if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
		return 0, ErrKVConflict
	}
	return next, err
}

func (b *KVBucket) Delete(ctx context.Context, key string, revision uint64) error {
	err := b.kv.Delete(ctx, key, jetstream.LastRevision(revision))
	if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
		return ErrKVConflict
	}
	return err
}

// Watch sends every current value, then nil once they are all delivered, then
// each later change, until ctx ends.
func (b *KVBucket) Watch(ctx context.Context) (<-chan *KVEvent, error) {
	w, err := b.kv.WatchAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make(chan *KVEvent, 256)
	go func() {
		defer close(out)
		defer func() { _ = w.Stop() }()
		for {
			select {
			case <-ctx.Done():
				return
			case entry, ok := <-w.Updates():
				if !ok {
					return
				}
				var event *KVEvent
				if entry != nil {
					event = &KVEvent{Key: entry.Key(), Revision: entry.Revision()}
					if entry.Operation() == jetstream.KeyValuePut {
						event.Value = entry.Value()
					}
				}
				select {
				case out <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}
