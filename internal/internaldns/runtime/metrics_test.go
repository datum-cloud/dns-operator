package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestRetryLoopReportsFailureAndRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id := loopID{Loop: t.Name()}
	operation := id.operation()
	defer operationResults.DeleteLabelValues(operation, "error")
	defer operationResults.DeleteLabelValues(operation, "success")
	defer operationLastSuccess.DeleteLabelValues(operation)
	done := make(chan error, 1)
	calls := 0
	go retryLoop(ctx, time.Millisecond, id, nil, func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("source unavailable")
		}
		cancel()
		return nil
	}, done)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop")
	}
	if got := testutil.ToFloat64(operationResults.WithLabelValues(operation, "error")); got != 1 {
		t.Fatalf("error count = %v", got)
	}
	if got := testutil.ToFloat64(operationResults.WithLabelValues(operation, "success")); got != 1 {
		t.Fatalf("success count = %v", got)
	}
	if got := testutil.ToFloat64(operationLastSuccess.WithLabelValues(operation)); got == 0 {
		t.Fatal("successful recovery did not update freshness")
	}
}

func TestFailedOperationDoesNotRenewSuccess(t *testing.T) {
	id := loopID{Loop: t.Name()}
	operation := id.operation()
	defer operationResults.DeleteLabelValues(operation, "error")
	defer operationLastSuccess.DeleteLabelValues(operation)
	operationLastSuccess.WithLabelValues(operation).Set(123)
	loopLastSuccess.WithLabelValues(id.Loop, "", "").Set(123)
	countLoop(id, errors.New("failed"))
	if got := testutil.ToFloat64(operationLastSuccess.WithLabelValues(operation)); got != 123 {
		t.Fatalf("failure renewed successful operation timestamp: %v", got)
	}
	if got := testutil.ToFloat64(loopLastSuccess.WithLabelValues(id.Loop, "", "")); got != 123 {
		t.Fatalf("failure renewed loop success timestamp: %v", got)
	}
}

func TestConflictIsNotAFailure(t *testing.T) {
	id := loopID{Loop: t.Name(), Region: "r1", Shard: "s1"}
	conflict := apierrors.NewConflict(schema.GroupResource{Resource: "dnstransportoutboxes"}, "o", errors.New("modified"))
	countLoop(id, errors.New("unavailable"))
	countLoop(id, fmt.Errorf("update outbox: %w", conflict))
	if got := testutil.ToFloat64(loopRuns.WithLabelValues(id.Loop, "r1", "s1", "conflict")); got != 1 {
		t.Fatalf("conflict runs = %v", got)
	}
	if got := testutil.ToFloat64(loopConsecutiveFailures.WithLabelValues(id.Loop, "r1", "s1")); got != 1 {
		t.Fatalf("a conflict changed consecutive failures to %v", got)
	}
	countLoop(id, nil)
	if got := testutil.ToFloat64(loopConsecutiveFailures.WithLabelValues(id.Loop, "r1", "s1")); got != 0 {
		t.Fatalf("success left consecutive failures at %v", got)
	}
	if got := testutil.ToFloat64(operationResults.WithLabelValues(id.operation(), "error")); got != 2 {
		t.Fatalf("deprecated error count = %v, want conflicts still counted", got)
	}
}

func TestJoinedRealFailureIsAnError(t *testing.T) {
	conflict := apierrors.NewConflict(schema.GroupResource{Resource: "dnsresolverbindings"}, "b", errors.New("modified"))
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"conflict":            {conflict, "conflict"},
		"wrapped conflict":    {fmt.Errorf("refresh: %w", conflict), "conflict"},
		"joined conflicts":    {errors.Join(conflict, fmt.Errorf("zone: %w", conflict)), "conflict"},
		"conflict and outage": {errors.Join(conflict, errors.New("timeout")), "error"},
		"wrapped mixed join":  {fmt.Errorf("pass: %w", errors.Join(errors.New("timeout"), conflict)), "error"},
		"outage":              {errors.New("timeout"), "error"},
	} {
		if got := loopResult(tc.err); got != tc.want {
			t.Errorf("%s: result = %q, want %q", name, got, tc.want)
		}
	}
}

func TestOperationKeepsDeprecatedNames(t *testing.T) {
	for id, want := range map[loopID]string{
		{Loop: "project discovery"}:                                  "project discovery",
		{Loop: "outbox", Region: "us-central-1"}:                     "outbox us-central-1",
		{Loop: "planner", Region: "us-central-1", Shard: "shared-0"}: "planner us-central-1/shared-0",
	} {
		if got := id.operation(); got != want {
			t.Errorf("%+v operation = %q, want %q", id, got, want)
		}
	}
}

func TestLoopLivenessReportsStalledLoop(t *testing.T) {
	now := time.Unix(1000, 0)
	live := newLoopLiveness()
	live.now = func() time.Time { return now }
	id := loopID{Loop: "outbox", Region: "r1"}
	live.start(id, 500*time.Millisecond)
	now = now.Add(minLoopStall)
	if err := live.check(nil); err != nil {
		t.Fatalf("loop within its limit reported stalled: %v", err)
	}
	now = now.Add(time.Second)
	if err := live.check(nil); err == nil {
		t.Fatal("stalled loop passed liveness")
	}
	live.tick(id)
	if err := live.check(nil); err != nil {
		t.Fatalf("loop that ticked reported stalled: %v", err)
	}
	now = now.Add(time.Hour)
	live.stop(id)
	if err := live.check(nil); err != nil {
		t.Fatalf("stopped loop reported stalled: %v", err)
	}
}
