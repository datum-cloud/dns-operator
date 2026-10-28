// SPDX-License-Identifier: AGPL-3.0-only

package transport

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type memoryEntry struct {
	key      string
	value    []byte
	revision uint64
	created  time.Time
}

func (e memoryEntry) Bucket() string                  { return "snapshots" }
func (e memoryEntry) Key() string                     { return e.key }
func (e memoryEntry) Value() []byte                   { return e.value }
func (e memoryEntry) Revision() uint64                { return e.revision }
func (e memoryEntry) Created() time.Time              { return e.created }
func (e memoryEntry) Delta() uint64                   { return 0 }
func (e memoryEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValuePut }

type memoryKeyValue struct {
	jetstream.KeyValue
	entries map[string]memoryEntry
	reads   []string
	now     time.Time
	next    uint64
}

func newMemoryKeyValue(now time.Time) *memoryKeyValue {
	return &memoryKeyValue{entries: map[string]memoryEntry{}, now: now}
}

func (m *memoryKeyValue) Get(_ context.Context, key string) (jetstream.KeyValueEntry, error) {
	m.reads = append(m.reads, key)
	e, ok := m.entries[key]
	if !ok {
		return nil, jetstream.ErrKeyNotFound
	}
	return e, nil
}

func (m *memoryKeyValue) Create(_ context.Context, key string, value []byte, _ ...jetstream.KVCreateOpt) (uint64, error) {
	if _, ok := m.entries[key]; ok {
		return 0, jetstream.ErrKeyExists
	}
	m.next++
	m.entries[key] = memoryEntry{key: key, value: value, revision: m.next, created: m.now}
	return m.next, nil
}

func (m *memoryKeyValue) Update(_ context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if m.entries[key].revision != revision {
		return 0, jetstream.ErrKeyRevisionMismatch
	}
	m.next++
	m.entries[key] = memoryEntry{key: key, value: value, revision: m.next, created: m.now}
	return m.next, nil
}

func (m *memoryKeyValue) Purge(_ context.Context, key string, _ ...jetstream.KVDeleteOpt) error {
	delete(m.entries, key)
	return nil
}

func (m *memoryKeyValue) ListKeysFiltered(_ context.Context, filters ...string) (jetstream.KeyLister, error) {
	var keys []string
	for key := range m.entries {
		for _, f := range filters {
			if strings.HasPrefix(key, strings.TrimSuffix(f, ">")) {
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	ch := make(chan string, len(keys))
	for _, k := range keys {
		ch <- k
	}
	close(ch)
	return memoryLister{ch}, nil
}

type memoryLister struct{ keys chan string }

func (l memoryLister) Keys() <-chan string { return l.keys }
func (l memoryLister) Stop() error         { return nil }

var snapshotTime = time.Date(2026, 10, 25, 22, 0, 0, 0, time.UTC)

func snapshotEnvelope(t *testing.T, kind model.Kind, zone string, epoch, revision uint64, payload any) model.Envelope {
	t.Helper()
	env, err := model.NewEnvelope(kind, fmt.Sprintf("%s-%s-%d-%d", kind, zone, epoch, revision), "central", "shard", zone, epoch, revision, snapshotTime, payload)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func putChunk(t *testing.T, store *SnapshotStore, zone string, epoch, revision uint64) string {
	t.Helper()
	env := snapshotEnvelope(t, model.KindPublicationChunk, zone, epoch, revision, model.PublicationChunk{ManifestUID: fmt.Sprintf("m-%d-%d", epoch, revision), ZoneUID: zone, WriterEpoch: epoch, Revision: revision, Index: 0, SHA256: strings.Repeat("a", 64), Payload: []byte("x")})
	key, err := model.SnapshotKey(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), key, env); err != nil {
		t.Fatal(err)
	}
	return key
}

func putManifest(t *testing.T, store *SnapshotStore, zone string, epoch, revision uint64) {
	t.Helper()
	env := snapshotEnvelope(t, model.KindPublicationManifest, zone, epoch, revision, map[string]any{"manifestUID": fmt.Sprintf("m-%d-%d", epoch, revision)})
	key, err := model.SnapshotKey(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), key, env); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotListReadsOnlyChunksOfManifestHeads(t *testing.T) {
	kv := newMemoryKeyValue(time.Now())
	store := &SnapshotStore{kv: kv}
	old := putChunk(t, store, "zone-a", 1, 1)
	current := putChunk(t, store, "zone-a", 1, 2)
	pending := putChunk(t, store, "zone-a", 1, 3)
	putManifest(t, store, "zone-a", 1, 2)
	other := putChunk(t, store, "zone-b", 4, 1)
	putManifest(t, store, "zone-b", 4, 1)

	kv.reads = nil
	envs, err := store.List(context.Background(), model.SnapshotPrefix("central", "shard"))
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 4 {
		t.Fatalf("listed %d envelopes, want two manifests and their two chunks", len(envs))
	}
	for _, read := range kv.reads {
		if read == old || read == pending {
			t.Fatalf("read chunk %s that no manifest head references", read)
		}
	}
	read := strings.Join(kv.reads, " ")
	if !strings.Contains(read, current) || !strings.Contains(read, other) {
		t.Fatalf("did not read the current chunks: %v", kv.reads)
	}
}

func TestSnapshotManifestKeepsTheReplacedHeadsChunks(t *testing.T) {
	kv := newMemoryKeyValue(time.Now())
	store := &SnapshotStore{kv: kv}
	first := putChunk(t, store, "zone-a", 1, 1)
	second := putChunk(t, store, "zone-a", 1, 2)
	third := putChunk(t, store, "zone-a", 1, 3)
	otherZone := putChunk(t, store, "zone-b", 1, 1)
	putManifest(t, store, "zone-a", 1, 2)
	putManifest(t, store, "zone-a", 1, 2)
	if len(kv.entries) != 5 {
		t.Fatalf("a first or repeated head purged chunks: %d keys left", len(kv.entries))
	}

	putManifest(t, store, "zone-a", 1, 3)
	for key, kept := range map[string]bool{first: false, second: true, third: true, otherZone: true} {
		if _, ok := kv.entries[key]; ok != kept {
			t.Errorf("after moving to r3, %s kept=%v, want %v", key, ok, kept)
		}
	}

	next := putChunk(t, store, "zone-a", 2, 1)
	putManifest(t, store, "zone-a", 2, 1)
	for key, kept := range map[string]bool{second: false, third: true, next: true, otherZone: true} {
		if _, ok := kv.entries[key]; ok != kept {
			t.Errorf("after moving to a new epoch, %s kept=%v, want %v", key, ok, kept)
		}
	}

	putManifest(t, store, "zone-a", 1, 9)
	if _, ok := kv.entries[third]; !ok {
		t.Fatal("a delayed older head purged the current head's predecessor")
	}
}

func TestSnapshotListOfEmptyFilteredBucketIsEmpty(t *testing.T) {
	envs, err := (&SnapshotStore{kv: newMemoryKeyValue(time.Now())}).List(context.Background(), "central.")
	if err != nil || len(envs) != 0 {
		t.Fatalf("empty bucket must list no snapshots, got %v, %v", envs, err)
	}
}
