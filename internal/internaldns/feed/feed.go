// SPDX-License-Identifier: AGPL-3.0-only

// Package feed carries the private DNS inputs a region writes itself to the
// other regions over NATS, without the project API or the federation hub in
// between.
//
// A region's publisher keeps one key per local-origin object in the region's
// own input bucket. Every other region's broker mirrors that bucket over a
// JetStream domain link, and that region's applier keeps a local copy of each
// object, marked with the region it came from. A mirror carries deletes as
// well as writes, and the applier removes copies whose key is gone, so a
// region converges on the writer's set even after missing events.
package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.miloapis.com/dns-operator/internal/internaldns/transport"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	group   = "dns.networking.miloapis.com"
	version = "v1alpha1"

	// OriginLabel names the region that wrote an object. It is the same
	// label the private replicator uses for the project API's objects.
	OriginLabel = "internal-dns.miloapis.com/origin"
	// OriginGenerationAnnotation is the object's generation in the region
	// that wrote it.
	OriginGenerationAnnotation = "internal-dns.miloapis.com/origin-generation"
)

// Kinds are the inputs a region may write itself.
var Kinds = []schema.GroupVersionKind{
	{Group: group, Version: version, Kind: "DNSRegistration"},
	{Group: group, Version: version, Kind: "DNSRecordContribution"},
}

// KV is the compare-and-swap bucket an input feed lives in.
type KV interface {
	Get(ctx context.Context, key string) ([]byte, uint64, error)
	Create(ctx context.Context, key string, value []byte) (uint64, error)
	Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error)
	Delete(ctx context.Context, key string, revision uint64) error
	Watch(ctx context.Context) (<-chan *transport.KVEvent, error)
}

// Entry is one local-origin object as the feed carries it.
type Entry struct {
	Kind        string            `json:"kind"`
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	Generation  int64             `json:"generation"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Spec        json.RawMessage   `json:"spec"`
}

// Key is the bucket key for an object: kind, namespace and name, which are
// all valid key tokens.
func Key(kind, namespace, name string) string {
	return kind + "." + namespace + "." + name
}

func parseKey(key string) (kind, namespace, name string, ok bool) {
	parts := strings.SplitN(key, ".", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func known(kind string) (schema.GroupVersionKind, bool) {
	for _, k := range Kinds {
		if k.Kind == kind {
			return k, true
		}
	}
	return schema.GroupVersionKind{}, false
}

func entryOf(obj *unstructured.Unstructured) (Entry, error) {
	spec, err := json.Marshal(obj.Object["spec"])
	if err != nil {
		return Entry{}, err
	}
	annotations := map[string]string{}
	for k, v := range obj.GetAnnotations() {
		if k != "kubectl.kubernetes.io/last-applied-configuration" {
			annotations[k] = v
		}
	}
	return Entry{Kind: obj.GetKind(), Namespace: obj.GetNamespace(), Name: obj.GetName(), Generation: obj.GetGeneration(), Labels: obj.GetLabels(), Annotations: annotations, Spec: spec}, nil
}

// put writes value under key, replacing whatever revision is there.
func put(ctx context.Context, kv KV, key string, value []byte) error {
	current, revision, err := kv.Get(ctx, key)
	if errors.Is(err, transport.ErrKVNotFound) {
		if _, err := kv.Create(ctx, key, value); err != nil && !errors.Is(err, transport.ErrKVExists) {
			return err
		} else if err == nil {
			return nil
		}
		current, revision, err = kv.Get(ctx, key)
	}
	if err != nil {
		return err
	}
	if string(current) == string(value) {
		return nil
	}
	if _, err := kv.Update(ctx, key, value, revision); err != nil {
		return fmt.Errorf("update %s: %w", key, err)
	}
	return nil
}

func remove(ctx context.Context, kv KV, key string) error {
	_, revision, err := kv.Get(ctx, key)
	if errors.Is(err, transport.ErrKVNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := kv.Delete(ctx, key, revision); err != nil && !errors.Is(err, transport.ErrKVNotFound) {
		return err
	}
	return nil
}
