// SPDX-License-Identifier: AGPL-3.0-only

package pubstore

import (
	"context"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var storeRequest = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "internal_dns_publication_store_request_seconds",
	Help:    "Latency of publication store bucket requests, by operation, record kind and result.",
	Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 5},
}, []string{"operation", "kind", "result"})

func init() {
	metrics.Registry.MustRegister(storeRequest)
}

// Instrumented records the latency of every request kv serves.
func Instrumented(kv KV) KV { return instrumentedKV{kv} }

type instrumentedKV struct{ KV }

// recordKind names the kind of record a bucket key holds, from its prefix.
func recordKind(key string) string {
	for _, kind := range storedKinds {
		if strings.HasPrefix(key, kind.prefix) {
			return strings.TrimSuffix(kind.prefix, ".")
		}
	}
	return "other"
}

func observe(operation, kind string, started time.Time, err error) {
	result := "ok"
	switch err {
	case nil:
	case transport.ErrKVNotFound:
		result = "not_found"
	case transport.ErrKVExists, transport.ErrKVConflict:
		result = "conflict"
	default:
		result = "error"
	}
	storeRequest.WithLabelValues(operation, kind, result).Observe(time.Since(started).Seconds())
}

func (k instrumentedKV) Get(ctx context.Context, key string) ([]byte, uint64, error) {
	started := time.Now()
	v, rev, err := k.KV.Get(ctx, key)
	observe("get", recordKind(key), started, err)
	return v, rev, err
}

func (k instrumentedKV) Create(ctx context.Context, key string, value []byte) (uint64, error) {
	started := time.Now()
	rev, err := k.KV.Create(ctx, key, value)
	observe("create", recordKind(key), started, err)
	return rev, err
}

func (k instrumentedKV) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	started := time.Now()
	rev, err := k.KV.Update(ctx, key, value, revision)
	observe("update", recordKind(key), started, err)
	return rev, err
}

func (k instrumentedKV) Delete(ctx context.Context, key string, revision uint64) error {
	started := time.Now()
	err := k.KV.Delete(ctx, key, revision)
	observe("delete", recordKind(key), started, err)
	return err
}

func (k instrumentedKV) PurgeDeletes(ctx context.Context, age time.Duration) error {
	purger, ok := k.KV.(interface {
		PurgeDeletes(context.Context, time.Duration) error
	})
	if !ok {
		return nil
	}
	started := time.Now()
	err := purger.PurgeDeletes(ctx, age)
	observe("purge", "all", started, err)
	return err
}
