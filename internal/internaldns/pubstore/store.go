// SPDX-License-Identifier: AGPL-3.0-only

package pubstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/watch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const shardOwnerPrefix = "dns-shard-owner-"

// KV is the compare-and-swap bucket the store keeps its records in.
type KV interface {
	Get(ctx context.Context, key string) ([]byte, uint64, error)
	Create(ctx context.Context, key string, value []byte) (uint64, error)
	Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error)
	Delete(ctx context.Context, key string, revision uint64) error
	Watch(ctx context.Context) (<-chan *transport.KVEvent, error)
}

type storedKind struct {
	prefix   string
	resource schema.GroupResource
	status   bool
	indexed  bool
	newObj   func() client.Object
}

var (
	ownershipKind = &storedKind{prefix: "own.", resource: dnsResource("dnspublicationownerships"), indexed: true, newObj: func() client.Object { return &dnsv1.DNSPublicationOwnership{} }}
	manifestKind  = &storedKind{prefix: "man.", resource: dnsResource("dnspublicationmanifests"), status: true, indexed: true, newObj: func() client.Object { return &dnsv1.DNSPublicationManifest{} }}
	chunkKind     = &storedKind{prefix: "chk.", resource: dnsResource("dnspublicationchunks"), newObj: func() client.Object { return &dnsv1.DNSPublicationChunk{} }}
	outboxKind    = &storedKind{prefix: "out.", resource: dnsResource("dnstransportoutboxes"), status: true, indexed: true, newObj: func() client.Object { return &dnsv1.DNSTransportOutbox{} }}
	shardKind     = &storedKind{prefix: "shd.", resource: schema.GroupResource{Resource: "configmaps"}, indexed: true, newObj: func() client.Object { return &corev1.ConfigMap{} }}
	storedKinds   = []*storedKind{ownershipKind, manifestKind, chunkKind, outboxKind, shardKind}
)

// WatchFilter names the keys the store's index follows. Chunks stay out of it:
// they are read only by name.
func WatchFilter() []string {
	var filter []string
	for _, kind := range storedKinds {
		if kind.indexed {
			filter = append(filter, kind.prefix+">")
		}
	}
	return filter
}

func dnsResource(resource string) schema.GroupResource {
	return schema.GroupResource{Group: dnsv1.GroupVersion.Group, Resource: resource}
}

// storedKindFor names the kind this store keeps for obj, or nil when the object
// stays in the API server. Shard ownership is the only ConfigMap kept here.
func storedKindFor(obj k8sruntime.Object, name string) *storedKind {
	switch obj.(type) {
	case *dnsv1.DNSPublicationOwnership, *dnsv1.DNSPublicationOwnershipList:
		return ownershipKind
	case *dnsv1.DNSPublicationManifest, *dnsv1.DNSPublicationManifestList:
		return manifestKind
	case *dnsv1.DNSPublicationChunk, *dnsv1.DNSPublicationChunkList:
		return chunkKind
	case *dnsv1.DNSTransportOutbox, *dnsv1.DNSTransportOutboxList:
		return outboxKind
	case *corev1.ConfigMap:
		if strings.HasPrefix(name, shardOwnerPrefix) {
			return shardKind
		}
	}
	return nil
}

type indexed struct {
	bucket   string
	revision uint64
	obj      client.Object
}

// PublicationStore keeps the control plane's own publication records
// (ownership leases, manifests, chunks, transport outboxes and shard ownership)
// in a JetStream key-value bucket and passes every other object to the API
// server. A resource version is the bucket revision, so creates, updates and
// optimistically locked patches keep the API server's compare-and-swap
// contract. Lists are served from an index fed by the bucket watches and by
// this client's own writes.
//
// Each region may have its own bucket. A record labelled with a region lives in
// that region's bucket, so publishing to a region needs only that region's
// broker; any other record lives in the home bucket. Record names are unique
// across buckets.
type PublicationStore struct {
	client.Client
	home    string
	buckets map[string]KV
	regions map[string]string
	now     func() time.Time

	lastPurge time.Time

	mu      sync.RWMutex
	index   map[string]indexed
	pending map[string]bool
	synced  chan struct{}
}

const regionLabel = "internal-dns.miloapis.com/region"

// New keeps every record in one bucket.
func New(api client.Client, kv KV) *PublicationStore {
	return NewRegional(api, "", map[string]KV{"": kv}, nil)
}

// NewRegional keeps outboxes and shard leases labelled with a region (by its
// safe token) in the bucket regions names for it, and every other record in
// the home bucket. Buckets are keyed by physical bucket, so regions that share
// a broker name the same one.
func NewRegional(api client.Client, home string, buckets map[string]KV, regions map[string]string) *PublicationStore {
	return &PublicationStore{Client: api, home: home, buckets: buckets, regions: regions, now: time.Now, index: map[string]indexed{}, synced: make(chan struct{})}
}

func regional(kind *storedKind) bool { return kind == outboxKind || kind == shardKind }

func (s *PublicationStore) bucketNames() []string {
	names := make([]string, 0, len(s.buckets))
	for name := range s.buckets {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == s.home) != (names[j] == s.home) {
			return names[i] == s.home
		}
		return names[i] < names[j]
	})
	return names
}

// bucketFor is where a new obj is written.
func (s *PublicationStore) bucketFor(kind *storedKind, obj client.Object) string {
	if regional(kind) {
		if bucket, ok := s.regions[obj.GetLabels()[regionLabel]]; ok {
			return bucket
		}
	}
	return s.home
}

// locate finds the bucket holding key. Zone leases, manifests and chunks are
// only ever in the home bucket. A regional record is where the index says;
// one the index does not know yet is looked up in every bucket at once, and is
// found as long as the bucket holding it answers.
func (s *PublicationStore) locate(ctx context.Context, kind *storedKind, key string) (string, []byte, uint64, error) {
	if !regional(kind) {
		value, revision, err := s.buckets[s.home].Get(ctx, key)
		return s.home, value, revision, err
	}
	s.mu.RLock()
	entry, ok := s.index[key]
	s.mu.RUnlock()
	if ok {
		value, revision, err := s.buckets[entry.bucket].Get(ctx, key)
		if !errors.Is(err, transport.ErrKVNotFound) {
			return entry.bucket, value, revision, err
		}
	}
	type found struct {
		bucket   string
		value    []byte
		revision uint64
		err      error
	}
	names := s.bucketNames()
	results := make(chan found, len(names))
	for _, name := range names {
		go func() {
			value, revision, err := s.buckets[name].Get(ctx, key)
			results <- found{name, value, revision, err}
		}()
	}
	var failed error
	for range names {
		r := <-results
		switch {
		case r.err == nil:
			return r.bucket, r.value, r.revision, nil
		case !errors.Is(r.err, transport.ErrKVNotFound):
			failed = r.err
		}
	}
	if failed != nil {
		return "", nil, 0, failed
	}
	return "", nil, 0, transport.ErrKVNotFound
}

// Start watches every bucket until ctx ends and returns once the index holds
// every record of the home bucket. A regional bucket whose broker is not
// reachable yet is watched as soon as it is, so one region's broker never holds
// up the others.
func (s *PublicationStore) Start(ctx context.Context) error {
	s.pending = map[string]bool{}
	for name := range s.buckets {
		s.pending[name] = true
	}
	homeReady := make(chan error, 1)
	for _, name := range s.bucketNames() {
		ready := make(chan error, 1)
		if name == s.home {
			ready = homeReady
		}
		go func() {
			for ctx.Err() == nil {
				events, err := s.buckets[name].Watch(ctx)
				if err == nil {
					s.follow(ctx, name, events, ready)
					return
				}
				if name == s.home {
					ready <- err
					return
				}
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
				}
			}
		}()
	}
	select {
	case err := <-homeReady:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	close(s.synced)
	return nil
}

// complete reports whether the index holds every bucket's records.
func (s *PublicationStore) complete() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.pending) == 0
}

func (s *PublicationStore) follow(ctx context.Context, bucket string, events <-chan *transport.KVEvent, ready chan<- error) {
	log := ctrl.LoggerFrom(ctx).WithName("internal-dns-publication-store")
	first := true
	for ctx.Err() == nil {
		seen := map[string]bool{}
		var highest uint64
		replaying := true
		for event := range events {
			if event.Synced {
				if replaying {
					s.dropUnseen(bucket, seen, max(highest, event.Revision))
					replaying = false
					if first {
						first = false
						s.mu.Lock()
						delete(s.pending, bucket)
						s.mu.Unlock()
						ready <- nil
					}
				}
				continue
			}
			if replaying {
				seen[event.Key] = true
				highest = max(highest, event.Revision)
			}
			s.apply(bucket, event.Key, event.Revision, event.Value)
		}
		if ctx.Err() != nil {
			return
		}
		log.Info("publication store watch ended; rewatching", "bucket", bucket)
		for ctx.Err() == nil {
			var err error
			if events, err = s.buckets[bucket].Watch(ctx); err == nil {
				break
			}
			log.Error(err, "publication store rewatch failed")
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

// dropUnseen removes records deleted while the watch was down: a key the
// replay did not carry and that no write after the replay has touched.
func (s *PublicationStore) dropUnseen(bucket string, seen map[string]bool, highest uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.index {
		if entry.bucket == bucket && !seen[key] && entry.revision <= highest {
			delete(s.index, key)
		}
	}
}

func kindForKey(key string) *storedKind {
	for _, kind := range storedKinds {
		if strings.HasPrefix(key, kind.prefix) {
			return kind
		}
	}
	return nil
}

// apply records a bucket value in the index. A settled outbox is kept without
// its payload, because nothing sends it again; a listed settled outbox
// therefore carries no payload.
func (s *PublicationStore) apply(bucket, key string, revision uint64, value []byte) {
	kind := kindForKey(key)
	if kind == nil || !kind.indexed {
		return
	}
	var obj client.Object
	if value != nil {
		obj = kind.newObj()
		if err := decodeStored(value, revision, obj); err != nil {
			return
		}
		if out, ok := obj.(*dnsv1.DNSTransportOutbox); ok && settled(out) {
			out.Spec.Payload = nil
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.index[key]; ok && current.bucket == bucket && current.revision >= revision {
		return
	}
	if obj == nil {
		if current, ok := s.index[key]; ok && current.bucket != bucket {
			return
		}
		delete(s.index, key)
		return
	}
	s.index[key] = indexed{bucket: bucket, revision: revision, obj: obj}
}

func decodeStored(value []byte, revision uint64, obj client.Object) error {
	resetObject(obj)
	if err := json.Unmarshal(value, obj); err != nil {
		return err
	}
	obj.SetResourceVersion(strconv.FormatUint(revision, 10))
	return nil
}

func resetObject(obj client.Object) {
	v := reflect.ValueOf(obj).Elem()
	v.Set(reflect.Zero(v.Type()))
}

func (s *PublicationStore) key(kind *storedKind, namespace, name string) (string, error) {
	if namespace == "" || name == "" || strings.Contains(namespace, ".") {
		return "", fmt.Errorf("publication store requires a namespace and name, got %q/%q", namespace, name)
	}
	return kind.prefix + namespace + "." + name, nil
}

func (s *PublicationStore) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	kind := storedKindFor(obj, key.Name)
	if kind == nil {
		return s.Client.Get(ctx, key, obj, opts...)
	}
	k, err := s.key(kind, key.Namespace, key.Name)
	if err != nil {
		return err
	}
	bucket, value, revision, err := s.locate(ctx, kind, k)
	if errors.Is(err, transport.ErrKVNotFound) {
		return apierrors.NewNotFound(kind.resource, key.Name)
	}
	if err != nil {
		return err
	}
	s.apply(bucket, k, revision, value)
	return decodeStored(value, revision, obj)
}

func (s *PublicationStore) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	kind := storedKindFor(list, "")
	if kind == nil {
		return s.Client.List(ctx, list, opts...)
	}
	if !kind.indexed {
		return fmt.Errorf("publication store does not list %s", kind.resource)
	}
	lo := &client.ListOptions{}
	lo.ApplyOptions(opts)
	if lo.FieldSelector != nil {
		for _, r := range lo.FieldSelector.Requirements() {
			if _, ok := storedFields(kind.newObj())[r.Field]; !ok {
				return apierrors.NewBadRequest(fmt.Sprintf("field selector %q is not supported for %s", r.Field, kind.resource))
			}
		}
	}
	select {
	case <-s.synced:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.RLock()
	keys := make([]string, 0, len(s.index))
	for k := range s.index {
		if strings.HasPrefix(k, kind.prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	items := make([]k8sruntime.Object, 0, len(keys))
	for _, k := range keys {
		obj := s.index[k].obj
		if lo.Namespace != "" && obj.GetNamespace() != lo.Namespace {
			continue
		}
		if lo.LabelSelector != nil && !lo.LabelSelector.Matches(labels.Set(obj.GetLabels())) {
			continue
		}
		if lo.FieldSelector != nil && !lo.FieldSelector.Matches(storedFields(obj)) {
			continue
		}
		items = append(items, obj.DeepCopyObject())
	}
	s.mu.RUnlock()
	return apimeta.SetList(list, items)
}

func storedFields(obj client.Object) fields.Set {
	set := fields.Set{"metadata.name": obj.GetName(), "metadata.namespace": obj.GetNamespace()}
	if out, ok := obj.(*dnsv1.DNSTransportOutbox); ok {
		set["status.state"] = out.Status.State
	}
	return set
}

func (s *PublicationStore) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	kind := storedKindFor(obj, obj.GetName())
	if kind == nil {
		return s.Client.Create(ctx, obj, opts...)
	}
	k, err := s.key(kind, obj.GetNamespace(), obj.GetName())
	if err != nil {
		return err
	}
	if obj.GetResourceVersion() != "" {
		return apierrors.NewBadRequest("resourceVersion should not be set on objects to be created")
	}
	next := obj.DeepCopyObject().(client.Object)
	next.SetUID(uuid.NewUUID())
	next.SetCreationTimestamp(metav1.NewTime(s.now().UTC()))
	next.SetGeneration(1)
	next.SetManagedFields(nil)
	value, err := json.Marshal(next)
	if err != nil {
		return err
	}
	bucket := s.bucketFor(kind, next)
	s.mu.RLock()
	elsewhere, held := s.index[k]
	s.mu.RUnlock()
	if held && elsewhere.bucket != bucket {
		return apierrors.NewAlreadyExists(kind.resource, obj.GetName())
	}
	revision, err := s.buckets[bucket].Create(ctx, k, value)
	if errors.Is(err, transport.ErrKVExists) {
		return apierrors.NewAlreadyExists(kind.resource, obj.GetName())
	}
	if err != nil {
		return err
	}
	s.apply(bucket, k, revision, value)
	return decodeStored(value, revision, obj)
}

func (s *PublicationStore) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	kind := storedKindFor(obj, obj.GetName())
	if kind == nil {
		return s.Client.Update(ctx, obj, opts...)
	}
	conditional := obj.GetResourceVersion() != ""
	return s.write(ctx, kind, obj, "", func(map[string]any) (map[string]any, bool, error) {
		desired, err := toMap(obj)
		return desired, conditional, err
	})
}

func (s *PublicationStore) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	kind := storedKindFor(obj, obj.GetName())
	if kind == nil {
		return s.Client.Patch(ctx, obj, patch, opts...)
	}
	return s.patch(ctx, kind, obj, patch, "")
}

func (s *PublicationStore) patch(ctx context.Context, kind *storedKind, obj client.Object, patch client.Patch, subresource string) error {
	if patch.Type() != types.MergePatchType {
		return fmt.Errorf("publication store supports merge patches only, not %s", patch.Type())
	}
	data, err := patch.Data(obj)
	if err != nil {
		return err
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	meta, _ := fields["metadata"].(map[string]any)
	_, conditional := meta["resourceVersion"]
	return s.write(ctx, kind, obj, subresource, func(current map[string]any) (map[string]any, bool, error) {
		base, err := json.Marshal(current)
		if err != nil {
			return nil, false, err
		}
		merged, err := jsonpatch.MergePatch(base, data)
		if err != nil {
			return nil, false, apierrors.NewBadRequest(err.Error())
		}
		var out map[string]any
		return out, conditional, json.Unmarshal(merged, &out)
	})
}

// write applies change to the stored object under compare-and-swap. A
// conditional change (one that names a resource version) conflicts when the
// stored object moved; an unconditional one is reapplied to the newer object,
// as the API server does for a patch without a resource version.
func (s *PublicationStore) write(ctx context.Context, kind *storedKind, obj client.Object, subresource string, change func(current map[string]any) (map[string]any, bool, error)) error {
	k, err := s.key(kind, obj.GetNamespace(), obj.GetName())
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 16; attempt++ {
		bucket, value, revision, err := s.locate(ctx, kind, k)
		if errors.Is(err, transport.ErrKVNotFound) {
			return apierrors.NewNotFound(kind.resource, obj.GetName())
		}
		if err != nil {
			return err
		}
		stored := kind.newObj()
		if err := decodeStored(value, revision, stored); err != nil {
			return err
		}
		current, err := toMap(stored)
		if err != nil {
			return err
		}
		next, conditional, err := change(current)
		if err != nil {
			return err
		}
		if meta, _ := next["metadata"].(map[string]any); meta != nil {
			if v, _ := meta["resourceVersion"].(string); conditional && v != strconv.FormatUint(revision, 10) {
				return modified(kind, obj)
			}
			if uid, _ := meta["uid"].(string); uid != "" && uid != string(stored.GetUID()) {
				return apierrors.NewConflict(kind.resource, obj.GetName(), errors.New("the object UID does not match"))
			}
		}
		result := compose(kind, current, next, subresource)
		desired := kind.newObj()
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, desired); err != nil {
			return apierrors.NewBadRequest(err.Error())
		}
		desired.SetName(stored.GetName())
		desired.SetNamespace(stored.GetNamespace())
		desired.SetUID(stored.GetUID())
		desired.SetCreationTimestamp(stored.GetCreationTimestamp())
		desired.SetResourceVersion("")
		desired.SetManagedFields(nil)
		desired.SetGeneration(stored.GetGeneration())
		if specChanged(stored, desired) {
			desired.SetGeneration(stored.GetGeneration() + 1)
		}
		out, err := json.Marshal(desired)
		if err != nil {
			return err
		}
		written, err := s.buckets[bucket].Update(ctx, k, out, revision)
		if errors.Is(err, transport.ErrKVConflict) {
			if conditional {
				return modified(kind, obj)
			}
			continue
		}
		if err != nil {
			return err
		}
		s.apply(bucket, k, written, out)
		return decodeStored(out, written, obj)
	}
	return modified(kind, obj)
}

func modified(kind *storedKind, obj client.Object) error {
	return apierrors.NewConflict(kind.resource, obj.GetName(), errors.New("the object has been modified; please apply your changes to the latest version and try again"))
}

// compose keeps the status subresource contract: a main write never changes
// status and a status write changes nothing else.
func compose(kind *storedKind, current, next map[string]any, subresource string) map[string]any {
	switch {
	case subresource == "status":
		out := map[string]any{}
		for k, v := range current {
			out[k] = v
		}
		if v, ok := next["status"]; ok {
			out["status"] = v
		} else {
			delete(out, "status")
		}
		return out
	case kind.status:
		if v, ok := current["status"]; ok {
			next["status"] = v
		} else {
			delete(next, "status")
		}
	}
	return next
}

func specChanged(a, b client.Object) bool {
	am, _ := toMap(a)
	bm, _ := toMap(b)
	delete(am, "metadata")
	delete(bm, "metadata")
	delete(am, "status")
	delete(bm, "status")
	return !reflect.DeepEqual(am, bm)
}

func toMap(obj any) (map[string]any, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(b, &out)
}

func (s *PublicationStore) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	kind := storedKindFor(obj, obj.GetName())
	if kind == nil {
		return s.Client.Delete(ctx, obj, opts...)
	}
	k, err := s.key(kind, obj.GetNamespace(), obj.GetName())
	if err != nil {
		return err
	}
	do := &client.DeleteOptions{}
	do.ApplyOptions(opts)
	bucket, value, revision, err := s.locate(ctx, kind, k)
	if errors.Is(err, transport.ErrKVNotFound) {
		return apierrors.NewNotFound(kind.resource, obj.GetName())
	}
	if err != nil {
		return err
	}
	stored := kind.newObj()
	if err := decodeStored(value, revision, stored); err != nil {
		return err
	}
	if p := do.Preconditions; p != nil {
		if p.UID != nil && *p.UID != stored.GetUID() || p.ResourceVersion != nil && *p.ResourceVersion != stored.GetResourceVersion() {
			return apierrors.NewConflict(kind.resource, obj.GetName(), errors.New("precondition failed"))
		}
	}
	if err := s.buckets[bucket].Delete(ctx, k, revision); errors.Is(err, transport.ErrKVConflict) {
		return modified(kind, obj)
	} else if err != nil {
		return err
	}
	s.mu.Lock()
	if current, ok := s.index[k]; ok && current.bucket == bucket && current.revision <= revision {
		delete(s.index, k)
	}
	s.mu.Unlock()
	return nil
}

func (s *PublicationStore) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	if storedKindFor(obj, obj.GetName()) != nil {
		return errors.New("publication store does not support delete collection")
	}
	return s.Client.DeleteAllOf(ctx, obj, opts...)
}

func (s *PublicationStore) Status() client.SubResourceWriter {
	return &storeStatus{store: s}
}

func (s *PublicationStore) SubResource(name string) client.SubResourceClient {
	if name == "status" {
		return &storeStatus{store: s}
	}
	return s.Client.SubResource(name)
}

type storeStatus struct{ store *PublicationStore }

func (w *storeStatus) Get(ctx context.Context, obj client.Object, sub client.Object, opts ...client.SubResourceGetOption) error {
	if storedKindFor(obj, obj.GetName()) != nil {
		return errors.New("publication store has no status getter")
	}
	return w.store.Client.SubResource("status").Get(ctx, obj, sub, opts...)
}

func (w *storeStatus) Create(ctx context.Context, obj client.Object, sub client.Object, opts ...client.SubResourceCreateOption) error {
	if storedKindFor(obj, obj.GetName()) != nil {
		return errors.New("publication store cannot create a status")
	}
	return w.store.Client.SubResource("status").Create(ctx, obj, sub, opts...)
}

func (w *storeStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	kind := storedKindFor(obj, obj.GetName())
	if kind == nil {
		return w.store.Client.Status().Update(ctx, obj, opts...)
	}
	if !kind.status {
		return apierrors.NewNotFound(kind.resource, obj.GetName()+"/status")
	}
	conditional := obj.GetResourceVersion() != ""
	return w.store.write(ctx, kind, obj, "status", func(map[string]any) (map[string]any, bool, error) {
		desired, err := toMap(obj)
		return desired, conditional, err
	})
}

func (w *storeStatus) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	kind := storedKindFor(obj, obj.GetName())
	if kind == nil {
		return w.store.Client.Status().Patch(ctx, obj, patch, opts...)
	}
	if !kind.status {
		return apierrors.NewNotFound(kind.resource, obj.GetName()+"/status")
	}
	return w.store.patch(ctx, kind, obj, patch, "status")
}

func (w *storeStatus) Apply(ctx context.Context, obj k8sruntime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	return w.store.Client.Status().Apply(ctx, obj, opts...)
}

var _ client.Client = (*PublicationStore)(nil)

// Seed stores obj as given, keeping its UID and timestamps, for fixtures that
// start from existing records. It reports whether obj is a kind the store keeps.
func (s *PublicationStore) Seed(ctx context.Context, obj client.Object) (bool, error) {
	kind := storedKindFor(obj, obj.GetName())
	if kind == nil {
		return false, nil
	}
	k, err := s.key(kind, obj.GetNamespace(), obj.GetName())
	if err != nil {
		return true, err
	}
	next := obj.DeepCopyObject().(client.Object)
	next.SetResourceVersion("")
	if next.GetUID() == "" {
		next.SetUID(uuid.NewUUID())
	}
	value, err := json.Marshal(next)
	if err != nil {
		return true, err
	}
	bucket := s.bucketFor(kind, next)
	revision, err := s.buckets[bucket].Create(ctx, k, value)
	if err != nil {
		return true, err
	}
	s.apply(bucket, k, revision, value)
	return true, nil
}

// Watch passes through to the API server client for the kinds it serves.
func (s *PublicationStore) Watch(ctx context.Context, list client.ObjectList, opts ...client.ListOption) (watch.Interface, error) {
	if storedKindFor(list, "") != nil {
		return nil, errors.New("publication store does not serve watches")
	}
	w, ok := s.Client.(client.WithWatch)
	if !ok {
		return nil, errors.New("API client does not serve watches")
	}
	return w.Watch(ctx, list, opts...)
}
