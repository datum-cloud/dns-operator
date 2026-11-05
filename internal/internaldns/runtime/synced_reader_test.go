// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSyncedReaderRefusesAStoppedRunsCache(t *testing.T) {
	var none *syncedReader
	if none.get() != nil {
		t.Fatal("a missing holder returned a reader")
	}
	first, second := fake.NewClientBuilder().Build(), fake.NewClientBuilder().Build()
	r := &syncedReader{}
	run := r.begin()
	if r.get() != nil {
		t.Fatal("a run served reads before its cache synced")
	}
	if !r.install(run, first) || r.get() != first {
		t.Fatal("a synced cache was not installed")
	}
	r.end(run)
	if r.get() != nil {
		t.Fatal("a stopped run kept serving reads")
	}
	if r.install(run, first) || r.get() != nil {
		t.Fatal("a stopped run installed its cache late")
	}
	next := r.begin()
	if r.install(run, first) {
		t.Fatal("an earlier run installed its cache over the current one")
	}
	if !r.install(next, second) || r.get() != second {
		t.Fatal("the current run's cache was not installed")
	}
}
