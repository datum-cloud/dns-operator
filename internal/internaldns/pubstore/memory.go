// SPDX-License-Identifier: AGPL-3.0-only

package pubstore

import (
	"context"
	"sort"
	"sync"

	"go.miloapis.com/dns-operator/internal/internaldns/transport"
)

// MemoryKV is an in-process KV with the bucket's revision and watch semantics,
// for tests and local fixtures.
type MemoryKV struct {
	mu       sync.Mutex
	revision uint64
	values   map[string]memoryValue
	watchers []chan *transport.KVEvent
}

type memoryValue struct {
	value    []byte
	revision uint64
}

func NewMemoryKV() *MemoryKV { return &MemoryKV{values: map[string]memoryValue{}} }

func (m *MemoryKV) Get(_ context.Context, key string) ([]byte, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.values[key]
	if !ok {
		return nil, 0, transport.ErrKVNotFound
	}
	return append([]byte(nil), v.value...), v.revision, nil
}

func (m *MemoryKV) Create(_ context.Context, key string, value []byte) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.values[key]; ok {
		return 0, transport.ErrKVExists
	}
	return m.put(key, value), nil
}

func (m *MemoryKV) Update(_ context.Context, key string, value []byte, revision uint64) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.values[key]; !ok || v.revision != revision {
		return 0, transport.ErrKVConflict
	}
	return m.put(key, value), nil
}

func (m *MemoryKV) Delete(_ context.Context, key string, revision uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.values[key]; !ok || v.revision != revision {
		return transport.ErrKVConflict
	}
	m.revision++
	delete(m.values, key)
	m.notify(&transport.KVEvent{Key: key, Revision: m.revision})
	return nil
}

func (m *MemoryKV) put(key string, value []byte) uint64 {
	m.revision++
	m.values[key] = memoryValue{value: append([]byte(nil), value...), revision: m.revision}
	m.notify(&transport.KVEvent{Key: key, Value: append([]byte(nil), value...), Revision: m.revision})
	return m.revision
}

func (m *MemoryKV) notify(event *transport.KVEvent) {
	for _, w := range m.watchers {
		w <- event
	}
}

func (m *MemoryKV) Watch(ctx context.Context) (<-chan *transport.KVEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan *transport.KVEvent, 1<<16)
	keys := make([]string, 0, len(m.values))
	for k := range m.values {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m.values[keys[i]].revision < m.values[keys[j]].revision })
	for _, k := range keys {
		v := m.values[k]
		ch <- &transport.KVEvent{Key: k, Value: append([]byte(nil), v.value...), Revision: v.revision}
	}
	ch <- &transport.KVEvent{Synced: true, Revision: m.revision}
	m.watchers = append(m.watchers, ch)
	go func() {
		<-ctx.Done()
		m.mu.Lock()
		defer m.mu.Unlock()
		for i, w := range m.watchers {
			if w == ch {
				m.watchers = append(m.watchers[:i], m.watchers[i+1:]...)
				close(ch)
				break
			}
		}
	}()
	return ch, nil
}
