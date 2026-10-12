package serving

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type bootstrapLister struct {
	calls   int
	results [][]model.Envelope
}

func (l *bootstrapLister) List(context.Context, string) ([]model.Envelope, error) {
	l.calls++
	if len(l.results) == 0 {
		return nil, errors.New("empty")
	}
	i := l.calls - 1
	if i >= len(l.results) {
		i = len(l.results) - 1
	}
	return l.results[i], nil
}

func TestSnapshotBootstrapRetriesRacingManifestHead(t *testing.T) {
	t.Parallel()
	now := testSnapshotNow()
	chunk, manifest := testPublication(t, now)
	serving := testSnapshot(now)
	chunkEnv := envelope(t, model.KindPublicationChunk, "zone-a", 2, 4, chunk)
	manifestEnv := envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, manifest)
	servingEnv := envelope(t, model.KindServingSnapshot, "shard", 1, 1, serving)
	lister := &bootstrapLister{results: [][]model.Envelope{{manifestEnv, servingEnv}, {manifestEnv, chunkEnv, servingEnv}}}
	got, err := (SnapshotBootstrap{Store: lister, Region: "east", Shard: "s1", Attempts: 2}).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lister.calls != 2 || len(got) != 3 || got[0].Kind != model.KindPublicationChunk {
		t.Fatalf("calls=%d events=%v", lister.calls, got)
	}
}

func TestSnapshotBootstrapIgnoresUnreferencedHistoricalChunks(t *testing.T) {
	t.Parallel()
	now := testSnapshotNow()
	chunk, manifest := testPublication(t, now)
	historical := chunk
	historical.ManifestUID = "old-manifest"
	events := []model.Envelope{
		envelope(t, model.KindPublicationChunk, "zone-a", 2, 3, historical),
		envelope(t, model.KindPublicationChunk, "zone-a", 2, 4, chunk),
		envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, manifest),
		envelope(t, model.KindServingSnapshot, "shard", 1, 1, testSnapshot(now)),
	}
	lister := &bootstrapLister{results: [][]model.Envelope{events}}
	got, err := (SnapshotBootstrap{Store: lister, Region: "east", Shard: "s1"}).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("cold bootstrap retained %d events, want current chunk+manifest+serving", len(got))
	}
	if got[0].Kind != model.KindPublicationChunk || got[1].Kind != model.KindServingSnapshot || got[2].Kind != model.KindPublicationManifest {
		t.Fatalf("unsafe bootstrap order: %#v", got)
	}
}
func testSnapshotNow() time.Time { return time.Now().UTC() }
